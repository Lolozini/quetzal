package main

import (
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// activity tracks the number of live flows and beats a control-plane callback
// while any are active, so the server's idle timer (LastActiveAt) stays fresh —
// the only way to measure UDP activity, which /proc/net/tcp can't see.
type activity struct {
	beat     func()
	interval time.Duration
	mu       sync.Mutex
	n        int
}

func (a *activity) inc() { a.mu.Lock(); a.n++; a.mu.Unlock() }
func (a *activity) dec() {
	a.mu.Lock()
	if a.n > 0 {
		a.n--
	}
	a.mu.Unlock()
}
func (a *activity) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.n }

func (a *activity) run() {
	if a.beat == nil || a.interval <= 0 {
		return
	}
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for range t.C {
		if a.count() > 0 {
			a.beat()
		}
	}
}

// proxy forwards TCP and UDP traffic to a server's real workload (via its
// internal Service), waking it on a new flow and reporting activity.
type proxy struct {
	waker      *waker
	activity   *activity
	dialBudget time.Duration // how long to keep retrying the backend while it starts
}

func runProxy() {
	backend := os.Getenv("QUETZAL_BACKEND")
	wakeURL := os.Getenv("QUETZAL_WAKE_URL")
	activeURL := os.Getenv("QUETZAL_ACTIVE_URL")
	slug := os.Getenv("QUETZAL_WAKE_SLUG")
	token := os.Getenv("QUETZAL_WAKE_TOKEN")
	tcp := splitPorts(os.Getenv("QUETZAL_TCP_PORTS"))
	udp := splitPorts(os.Getenv("QUETZAL_UDP_PORTS"))
	if backend == "" || (len(tcp) == 0 && len(udp) == 0) {
		log.Fatalf("activator(proxy): QUETZAL_BACKEND and a TCP/UDP port are required")
	}
	act := &activity{interval: 30 * time.Second}
	if activeURL != "" {
		act.beat = func() { _ = postCallback(activeURL, slug, token) }
	}
	go act.run()
	p := &proxy{
		waker:      &waker{cooldown: 15 * time.Second, post: func() error { return postCallback(wakeURL, slug, token) }},
		activity:   act,
		dialBudget: 90 * time.Second,
	}
	for _, port := range tcp {
		ln, err := net.Listen("tcp", ":"+port)
		if err != nil {
			log.Fatalf("activator: tcp listen %s: %v", port, err)
		}
		go p.serveTCP(ln, net.JoinHostPort(backend, port))
	}
	for _, port := range udp {
		laddr, _ := net.ResolveUDPAddr("udp", ":"+port)
		pc, err := net.ListenUDP("udp", laddr)
		if err != nil {
			log.Fatalf("activator: udp listen %s: %v", port, err)
		}
		go p.serveUDP(pc, net.JoinHostPort(backend, port))
	}
	log.Printf("activator(proxy): fronting %q -> %s (tcp %v, udp %v)", slug, backend, tcp, udp)
	select {}
}

// ---- TCP ----

func (p *proxy) serveTCP(ln net.Listener, backendAddr string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.handleTCP(c, backendAddr)
	}
}

func (p *proxy) handleTCP(client net.Conn, backendAddr string) {
	defer client.Close()
	p.waker.trigger()
	p.activity.inc()
	defer p.activity.dec()
	be := p.dialTCP(backendAddr)
	if be == nil {
		return // server never came up within the budget; client retries
	}
	defer be.Close()
	pipe(client, be)
}

// dialTCP dials the backend, retrying within the budget while the woken server
// starts up. Returns nil if it never becomes reachable.
func (p *proxy) dialTCP(addr string) net.Conn {
	deadline := time.Now().Add(p.dialBudget)
	for {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			log.Printf("activator: backend %s unreachable within budget: %v", addr, err)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// pipe copies bidirectionally until either side closes, then closes both.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) { _, _ = io.Copy(dst, src); done <- struct{}{} }
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
}

// ---- UDP ----

type udpFlow struct {
	backend  *net.UDPConn
	lastSeen time.Time
}

const udpIdle = 60 * time.Second

func (p *proxy) serveUDP(pc *net.UDPConn, backendAddr string) {
	defer pc.Close()
	baddr, err := net.ResolveUDPAddr("udp", backendAddr)
	if err != nil {
		log.Printf("activator: resolve %s: %v", backendAddr, err)
		return
	}
	flows := map[string]*udpFlow{}
	var mu sync.Mutex

	go p.sweepUDP(flows, &mu)

	buf := make([]byte, 64*1024)
	for {
		n, caddr, err := pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		key := caddr.String()
		mu.Lock()
		f := flows[key]
		if f == nil {
			p.waker.trigger()
			be, derr := net.DialUDP("udp", nil, baddr)
			if derr != nil {
				mu.Unlock()
				continue
			}
			f = &udpFlow{backend: be, lastSeen: time.Now()}
			flows[key] = f
			p.activity.inc()
			go p.udpBackToClient(pc, be, caddr, flows, key, &mu)
		}
		f.lastSeen = time.Now()
		data := append([]byte(nil), buf[:n]...) // copy: buf is reused
		mu.Unlock()
		_, _ = f.backend.Write(data)
	}
}

// udpBackToClient relays the backend's responses for one flow back to the client.
func (p *proxy) udpBackToClient(pc *net.UDPConn, be *net.UDPConn, caddr *net.UDPAddr, flows map[string]*udpFlow, key string, mu *sync.Mutex) {
	buf := make([]byte, 64*1024)
	for {
		_ = be.SetReadDeadline(time.Now().Add(udpIdle + 10*time.Second))
		n, err := be.Read(buf)
		if err != nil {
			p.closeFlow(flows, key, be, mu)
			return
		}
		_, _ = pc.WriteToUDP(buf[:n], caddr)
		mu.Lock()
		if cur := flows[key]; cur != nil {
			cur.lastSeen = time.Now()
		}
		mu.Unlock()
	}
}

// sweepUDP expires idle flows.
func (p *proxy) sweepUDP(flows map[string]*udpFlow, mu *sync.Mutex) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
		mu.Lock()
		for k, f := range flows {
			if time.Since(f.lastSeen) > udpIdle {
				f.backend.Close()
				delete(flows, k)
				p.activity.dec()
			}
		}
		mu.Unlock()
	}
}

// closeFlow removes a flow exactly once (whoever wins the lock), decrementing
// the active count only if this backend socket is still the registered one.
func (p *proxy) closeFlow(flows map[string]*udpFlow, key string, be *net.UDPConn, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	if f := flows[key]; f != nil && f.backend == be {
		f.backend.Close()
		delete(flows, key)
		p.activity.dec()
	}
}
