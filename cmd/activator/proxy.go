package main

import (
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// activity beats a control-plane callback while traffic is flowing, so the
// server's idle timer (LastActiveAt) stays fresh — the only way to measure UDP
// activity, which /proc/net/tcp can't see.
//
// What counts is traffic, not open sockets. It used to be the number of live
// flows, and a TCP flow lives as long as nobody closes it: one silent
// connection -- a scanner that never hangs up, a client that vanished without a
// FIN -- beat the timer every interval and the server never hibernated again,
// for as long as the game itself tolerated the socket. (Minecraft does not: it
// drops a silent client after thirty seconds, and that close propagates through
// the pipe. Games without such a timeout had no way out.) UDP flows already
// expired after a minute of silence; TCP had no equivalent.
//
// Nothing is closed for being quiet. A player in a game without keepalives who
// stops typing keeps the connection; the server only sleeps under them once
// nobody at all has sent a byte for the whole hibernation window.
type activity struct {
	beat     func()
	interval time.Duration
	mu       sync.Mutex
	n        int // live flows, for accounting; no longer what drives the beat
	seen     atomic.Bool
}

// touch records that bytes moved on some flow since the last beat.
func (a *activity) touch() { a.seen.Store(true) }

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
		if a.seen.Swap(false) {
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
	// Shared across UDP listeners. Zero uses the production limit; tests may
	// lower it before serving. Slots include readers that are still closing.
	udpFlowLimit int32
	udpFlows     atomic.Int32
	// Limits on who may hold how many connections. Zero uses the production
	// value; tests lower them before serving. The pod has 64 MiB: a TCP flow
	// costs about 64 KiB of copy buffers and a UDP flow 64 KiB of reply buffer,
	// so without a ceiling a few thousand silent connections, from one address or
	// many, take the activator down and every player of the server with it.
	tcpConnLimit  int32
	tcpPerIPLimit int
	udpPerIPLimit int
	tcpConns      atomic.Int32
	tcpByIP       sourceCount
	udpByIP       sourceCount
	refusedLogged atomic.Int64 // when a refusal was last logged (Unix seconds)
	// gate says which flows are a player. For a Minecraft server only a login
	// on the game port wakes the server or counts as activity: a scanner's
	// server-list ping, a query packet or an RCON probe does neither.
	gate wakeGate
	// bedrock answers a Bedrock server list while the server sleeps.
	bedrock bedrockPong
}

// perAddressLimit reads the per-address limit a server asks for, 0 (the default)
// when it is unset or not a positive number.
func perAddressLimit(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0
	}
	return n
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
		gate:       gateFromEnv(),
	}
	if n := perAddressLimit(os.Getenv("QUETZAL_MAX_PER_ADDRESS")); n > 0 {
		p.tcpPerIPLimit, p.udpPerIPLimit = n, n
		log.Printf("activator(proxy): at most %d connections per source address", n)
	}
	for _, port := range tcp {
		ln, err := net.Listen("tcp", ":"+port)
		if err != nil {
			log.Fatalf("activator: tcp listen %s: %v", port, err)
		}
		go p.serveTCP(ln, net.JoinHostPort(backend, port), port)
	}
	for _, port := range udp {
		laddr, _ := net.ResolveUDPAddr("udp", ":"+port)
		pc, err := net.ListenUDP("udp", laddr)
		if err != nil {
			log.Fatalf("activator: udp listen %s: %v", port, err)
		}
		go p.serveUDP(pc, net.JoinHostPort(backend, port))
	}
	log.Printf("activator(proxy): fronting %q -> %s (tcp %v, udp %v), woken by %s", slug, backend, tcp, udp, p.gate)
	select {}
}

// ---- TCP ----

func (p *proxy) serveTCP(ln net.Listener, backendAddr, port string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		ip := sourceIP(c.RemoteAddr())
		if !p.acquireTCP(ip) {
			p.refused("tcp", ip)
			_ = c.Close()
			continue
		}
		var handle func()
		switch {
		case !p.gate.minecraft():
			handle = func() { p.handleTCP(c, backendAddr) }
		case port == p.gate.gamePort:
			handle = func() { p.handleMinecraft(c, backendAddr) }
		default:
			handle = func() { p.forwardIfUp(c, backendAddr) }
		}
		go func() {
			defer p.releaseTCP(ip)
			handle()
		}()
	}
}

// backendDial is how long a flow that must not wake the server waits for it.
const backendDial = 2 * time.Second

// handleMinecraft forwards a Minecraft client, but lets only a player joining
// wake the server or count as activity. While the server sleeps, a server-list
// ping is answered here and a player joining is told to reconnect in a minute:
// Minecraft's client gives up long before a server has started.
func (p *proxy) handleMinecraft(client net.Conn, backendAddr string) {
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(mcTimeout))
	h, err := readMCHandshake(client)
	if err != nil {
		return
	}
	_ = client.SetReadDeadline(time.Time{})
	if h.joining() {
		p.waker.trigger()
	}
	be, err := net.DialTimeout("tcp", backendAddr, backendDial)
	if err != nil {
		_ = client.SetDeadline(time.Now().Add(mcTimeout))
		if h.joining() {
			_ = mcDisconnect(client, mcWakingReason)
		} else {
			_ = serveMCStatus(client, h)
		}
		return
	}
	defer be.Close()
	if _, err := be.Write(h.raw); err != nil {
		return
	}
	if !h.joining() {
		pipe(client, be, nil)
		return
	}
	p.activity.inc()
	defer p.activity.dec()
	pipe(client, be, p.activity)
}

// forwardIfUp forwards a flow that is not a player -- RCON, a query port --
// when the server is up, and neither wakes it nor counts as activity.
func (p *proxy) forwardIfUp(client net.Conn, backendAddr string) {
	defer client.Close()
	be, err := net.DialTimeout("tcp", backendAddr, backendDial)
	if err != nil {
		return
	}
	defer be.Close()
	pipe(client, be, nil)
}

func (p *proxy) handleTCP(client net.Conn, backendAddr string) {
	defer client.Close()
	var first []byte
	be, err := net.DialTimeout("tcp", backendAddr, backendDial)
	if err == nil {
		// Awake: straight through, as it always went. The nudge does nothing
		// unless the server is on its way to sleep, which it then stops.
		p.waker.trigger()
	} else {
		// Asleep: a port scan wakes nothing (firstMove).
		var ok bool
		if ok, first = firstMove(client); !ok {
			return
		}
		p.waker.trigger()
		if be = p.dialTCP(backendAddr); be == nil {
			return // server never came up within the budget; client retries
		}
	}
	defer be.Close()
	p.activity.inc()
	defer p.activity.dec()
	if len(first) > 0 {
		if _, err := be.Write(first); err != nil {
			return
		}
		p.activity.touch()
	}
	pipe(client, be, p.activity)
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

// pipe copies bidirectionally until either side closes, then closes both,
// reporting every chunk that moves in either direction as activity.
func pipe(a, b net.Conn, act *activity) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, touchReader{r: src, act: act})
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
}

// touchReader reports every read that returned bytes as activity.
type touchReader struct {
	r   io.Reader
	act *activity
}

func (t touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.act != nil {
		t.act.touch()
	}
	return n, err
}

// ---- UDP ----

type udpFlow struct {
	backend  net.Conn
	lastSeen time.Time
	// player is set by the flow's first player packet (wakeGate.playerPacket):
	// until then it is a query, which neither wakes the server nor keeps it up.
	player bool
	// lastReply is when the server last answered on this flow.
	lastReply time.Time
}

const (
	udpIdle = 60 * time.Second
	// 128 response buffers occupy 8 MiB, leaving headroom in the 64 MiB pod
	// for the Go runtime, request buffers, sockets and TCP traffic.
	maxUDPFlows = 128
)

const (
	// maxTCPConns is how many TCP connections the proxy holds at once, whoever
	// opens them: 256 of them are about 16 MiB of the pod's 64.
	maxTCPConns = 256
	// maxTCPPerIP and maxUDPPerIP are how many one source address may hold by
	// default: the proxy's whole budget. Players behind a Velocity or BungeeCord
	// proxy, a NAT or a DDoS filter all arrive from one address, so a lower default
	// turns the thirty-third of them away. A server that wants an address held to
	// less sets QUETZAL_MAX_PER_ADDRESS (its "connections per address" setting).
	maxTCPPerIP = maxTCPConns
	maxUDPPerIP = maxUDPFlows
)

// sourceCount counts what each source address holds. The zero value is ready.
type sourceCount struct {
	mu sync.Mutex
	n  map[string]int
}

// acquire takes a slot for ip, or reports that it already holds limit of them.
func (c *sourceCount) acquire(ip string, limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	if c.n[ip] >= limit {
		return false
	}
	c.n[ip]++
	return true
}

func (c *sourceCount) release(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[ip] <= 1 {
		delete(c.n, ip)
		return
	}
	c.n[ip]--
}

// sourceIP names the source of a peer for counting: its address without the port,
// and for IPv6 the /64 it is in. An IPv6 subscriber is given a /64 and can take
// 2^64 addresses from it, so counting by whole address would let one of them fill
// the proxy as easily as no limit at all.
func sourceIP(a net.Addr) string {
	ip := sourceAddr(a)
	if v6 := net.ParseIP(ip); v6 != nil && v6.To4() == nil {
		return v6.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip
}

// sourceAddr is the address of a peer without its port (IPv6 included).
func sourceAddr(a net.Addr) string {
	switch v := a.(type) {
	case nil:
		return ""
	case *net.TCPAddr:
		if v == nil {
			return ""
		}
		return v.IP.String()
	case *net.UDPAddr:
		if v == nil {
			return ""
		}
		return v.IP.String()
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

func (p *proxy) acquireTCP(ip string) bool {
	limit := p.tcpConnLimit
	if limit <= 0 {
		limit = maxTCPConns
	}
	perIP := p.tcpPerIPLimit
	if perIP <= 0 {
		perIP = maxTCPPerIP
	}
	for {
		n := p.tcpConns.Load()
		if n >= limit {
			return false
		}
		if p.tcpConns.CompareAndSwap(n, n+1) {
			break
		}
	}
	if !p.tcpByIP.acquire(ip, perIP) {
		p.tcpConns.Add(-1)
		return false
	}
	return true
}

func (p *proxy) releaseTCP(ip string) {
	p.tcpByIP.release(ip)
	p.tcpConns.Add(-1)
}

// refused logs that a limit turned a connection away, at most once every 30
// seconds: under the flood the limit is there for, a line each would be the flood.
func (p *proxy) refused(kind, ip string) {
	now := time.Now().Unix()
	last := p.refusedLogged.Load()
	if now-last >= 30 && p.refusedLogged.CompareAndSwap(last, now) {
		log.Printf("activator: refusing %s connections (limit reached); the latest from %s", kind, ip)
	}
}

func (p *proxy) acquireUDPFlow() bool {
	limit := p.udpFlowLimit
	if limit <= 0 || limit > maxUDPFlows {
		limit = maxUDPFlows
	}
	for {
		n := p.udpFlows.Load()
		if n >= limit {
			return false
		}
		if p.udpFlows.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// pongWait is how long a RakNet ping waits for the server's own pong before the
// activator answers that it sleeps.
const pongWait = 500 * time.Millisecond

func (p *proxy) serveUDP(pc *net.UDPConn, backendAddr string) {
	defer pc.Close()
	flows := map[string]*udpFlow{}
	var mu sync.Mutex

	done := make(chan struct{})
	defer func() {
		close(done)
		mu.Lock()
		defer mu.Unlock()
		for key, f := range flows {
			_ = f.backend.Close()
			delete(flows, key)
			p.activity.dec()
		}
	}()
	go p.sweepUDP(flows, &mu, done)

	buf := make([]byte, 64*1024)
	for {
		n, caddr, err := pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		key := caddr.String()
		kind := classifyUDP(buf[:n])
		mu.Lock()
		f := flows[key]
		if f == nil {
			ip := sourceIP(caddr)
			perIP := p.udpPerIPLimit
			if perIP <= 0 {
				perIP = maxUDPPerIP
			}
			if !p.acquireUDPFlow() {
				mu.Unlock()
				continue
			}
			if !p.udpByIP.acquire(ip, perIP) {
				p.udpFlows.Add(-1)
				mu.Unlock()
				p.refused("udp", ip)
				continue
			}
			// Resolve + dial per flow so a transient DNS miss at startup (the
			// backend Service may not be resolvable yet) doesn't kill the handler.
			be, derr := net.Dial("udp", backendAddr)
			if derr != nil {
				p.udpFlows.Add(-1)
				p.udpByIP.release(ip)
				mu.Unlock()
				continue
			}
			f = &udpFlow{backend: be, lastSeen: time.Now()}
			flows[key] = f
			p.activity.inc()
			go p.udpBackToClient(pc, be, caddr, flows, key, &mu)
		}
		f.lastSeen = time.Now()
		// A query neither wakes the server nor keeps it up; the first player
		// packet of a flow wakes it, whether the flow opened with one or with
		// a query (a client refreshing the server list before joining).
		if !f.player && p.gate.playerPacket(kind) {
			f.player = true
			p.waker.trigger()
		}
		if f.player {
			p.activity.touch()
		}
		data := append([]byte(nil), buf[:n]...) // copy: buf is reused
		mu.Unlock()
		sent := time.Now()
		_, _ = f.backend.Write(data)
		if kind == packetPing {
			if pong := p.bedrock.asleep(data); pong != nil {
				time.AfterFunc(pongWait, func() {
					mu.Lock()
					answered := f.lastReply.After(sent)
					mu.Unlock()
					if !answered {
						_, _ = pc.WriteToUDP(pong, caddr)
					}
				})
			}
		}
	}
}

// udpBackToClient relays the backend's responses for one flow back to the client.
func (p *proxy) udpBackToClient(pc *net.UDPConn, be net.Conn, caddr *net.UDPAddr, flows map[string]*udpFlow, key string, mu *sync.Mutex) {
	// Only the reader releases the reservation: sweep/close may remove the
	// map entry earlier, while this goroutine still retains its buffer.
	ip := sourceIP(caddr)
	defer func() {
		p.udpByIP.release(ip)
		p.udpFlows.Add(-1)
	}()
	buf := make([]byte, 64*1024)
	for {
		_ = be.SetReadDeadline(time.Now().Add(udpIdle + 10*time.Second))
		n, err := be.Read(buf)
		if err != nil {
			p.closeFlow(flows, key, be, mu)
			return
		}
		_, _ = pc.WriteToUDP(buf[:n], caddr)
		p.bedrock.remember(buf[:n])
		mu.Lock()
		player := false
		if cur := flows[key]; cur != nil && cur.backend == be {
			cur.lastSeen = time.Now()
			cur.lastReply = cur.lastSeen
			player = cur.player
		}
		mu.Unlock()
		if player {
			p.activity.touch()
		}
	}
}

// sweepUDP expires idle flows.
func (p *proxy) sweepUDP(flows map[string]*udpFlow, mu *sync.Mutex, done <-chan struct{}) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
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
func (p *proxy) closeFlow(flows map[string]*udpFlow, key string, be net.Conn, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	if f := flows[key]; f != nil && f.backend == be {
		f.backend.Close()
		delete(flows, key)
		p.activity.dec()
	}
}
