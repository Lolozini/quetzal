package main

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestProxy(wakes, beats *int32) *proxy {
	act := &activity{interval: 20 * time.Millisecond, beat: func() { atomic.AddInt32(beats, 1) }}
	go act.run()
	return &proxy{
		waker:      &waker{cooldown: time.Hour, post: func() error { atomic.AddInt32(wakes, 1); return nil }},
		activity:   act,
		dialBudget: 3 * time.Second,
	}
}

func TestProxyTCP(t *testing.T) {
	// Backend TCP echo server.
	be, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer be.Close()
	go func() {
		for {
			c, err := be.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 256)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(buf[:n])
				}
			}(c)
		}
	}()

	var wakes, beats int32
	p := newTestProxy(&wakes, &beats)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("front: %v", err)
	}
	defer ln.Close()
	go p.serveTCP(ln, be.Addr().String(), "")

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("echo through proxy = %q, %v", string(buf[:n]), err)
	}
	if atomic.LoadInt32(&wakes) != 1 {
		t.Errorf("wakes = %d, want 1", atomic.LoadInt32(&wakes))
	}
	// Activity must have been counted (and beaten) while the flow was live.
	if waitFor(func() bool { return atomic.LoadInt32(&beats) > 0 }, time.Second) == false {
		t.Errorf("expected an activity heartbeat while a flow was live")
	}
}

func TestProxyUDP(t *testing.T) {
	// Backend UDP echo server.
	baddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	be, err := net.ListenUDP("udp", baddr)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer be.Close()
	go func() {
		buf := make([]byte, 1024)
		for {
			n, addr, err := be.ReadFromUDP(buf)
			if err != nil {
				return
			}
			be.WriteToUDP(buf[:n], addr)
		}
	}()

	var wakes, beats int32
	p := newTestProxy(&wakes, &beats)
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	front, err := net.ListenUDP("udp", laddr)
	if err != nil {
		t.Fatalf("front: %v", err)
	}
	defer front.Close()
	go p.serveUDP(front, be.LocalAddr().String())

	c, err := net.DialUDP("udp", nil, front.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("pong")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "pong" {
		t.Fatalf("echo through udp proxy = %q, %v", string(buf[:n]), err)
	}
	if atomic.LoadInt32(&wakes) != 1 {
		t.Errorf("udp wakes = %d, want 1", atomic.LoadInt32(&wakes))
	}
	if p.activity.count() != 1 {
		t.Errorf("udp active flows = %d, want 1", p.activity.count())
	}
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// Exercise admission at a small explicit budget rather than allocating the
// production maximum. Existing flows must still work after saturation.
func TestProxyUDPFlowLimit(t *testing.T) {
	be, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	go func() {
		buf := make([]byte, 1024)
		for {
			n, addr, err := be.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = be.WriteTo(buf[:n], addr)
		}
	}()
	const limit = 4
	p := &proxy{
		waker:        &waker{cooldown: time.Hour, post: func() error { return nil }},
		activity:     &activity{},
		udpFlowLimit: limit,
	}
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	go p.serveUDP(front, be.LocalAddr().String())
	var first *net.UDPConn
	buf := make([]byte, 32)
	for i := range limit + 1 {
		c, err := net.DialUDP("udp", nil, front.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if i == 0 {
			first = c
		}
		if _, err := c.Write([]byte("bounded")); err != nil {
			t.Fatal(err)
		}
		timeout := 5 * time.Second
		if i == limit {
			timeout = 200 * time.Millisecond
		}
		_ = c.SetReadDeadline(time.Now().Add(timeout))
		n, err := c.Read(buf)
		if i < limit && (err != nil || string(buf[:n]) != "bounded") {
			t.Fatalf("admitted flow %d lost: %v", i, err)
		}
		if i == limit && err == nil {
			t.Error("flow above limit reached backend")
		}
	}
	if got := p.activity.count(); got != limit {
		t.Errorf("live flows = %d, want %d", got, limit)
	}
	if _, err := first.Write([]byte("existing")); err != nil {
		t.Fatal(err)
	}
	_ = first.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := first.Read(buf)
	if err != nil || string(buf[:n]) != "existing" {
		t.Fatalf("existing flow lost at saturation: %v", err)
	}
}

func TestProxyUDPBudgetSharedAndReleased(t *testing.T) {
	be, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, addr, err := be.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = be.WriteTo(buf[:n], addr)
		}
	}()
	p := &proxy{
		waker:    &waker{cooldown: time.Hour, post: func() error { return nil }},
		activity: &activity{}, udpFlowLimit: 1,
	}
	listen := func() (*net.UDPConn, *net.UDPConn) {
		t.Helper()
		front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = front.Close() })
		go p.serveUDP(front, be.LocalAddr().String())
		c, err := net.DialUDP("udp", nil, front.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return front, c
	}
	frontA, a := listen()
	frontB, b := listen()
	exchange := func(c *net.UDPConn) error {
		t.Helper()
		if _, err := c.Write([]byte("echo")); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 32)
		n, err := c.Read(buf)
		if err == nil && string(buf[:n]) != "echo" {
			t.Fatalf("unexpected backend response %q", buf[:n])
		}
		return err
	}
	if err := exchange(a); err != nil {
		t.Fatal(err)
	}
	if err := exchange(b); err == nil {
		t.Error("another listener bypassed the proxy-wide budget")
	}
	if err := exchange(a); err != nil {
		t.Fatalf("admitted flow lost at saturation: %v", err)
	}
	_ = frontA.Close()
	if !waitFor(func() bool { return p.udpFlows.Load() == 0 && p.activity.count() == 0 }, time.Second) {
		t.Fatal("listener shutdown did not release its flow")
	}
	if err := exchange(b); err != nil {
		t.Fatalf("released slot could not be reused: %v", err)
	}
	if got := p.udpFlows.Load(); got != 1 {
		t.Errorf("reservation count = %d, want 1", got)
	}
	_ = frontB.Close()
	if !waitFor(func() bool { return p.udpFlows.Load() == 0 && p.activity.count() == 0 }, time.Second) {
		t.Fatal("flow was not released exactly once on shutdown")
	}
}

func TestUDPFlowConcurrentCloseReleasesOnce(t *testing.T) {
	p := &proxy{activity: &activity{}, udpFlowLimit: 1}
	flows := map[string]*udpFlow{}
	var mu sync.Mutex
	start := func() net.Conn {
		t.Helper()
		if !p.acquireUDPFlow() {
			t.Fatal("available slot could not be reserved")
		}
		be, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		mu.Lock()
		flows["client"] = &udpFlow{backend: be, lastSeen: time.Now()}
		p.activity.inc()
		mu.Unlock()
		go p.udpBackToClient(nil, be, nil, flows, "client", &mu)
		return be
	}
	be := start()
	var closers sync.WaitGroup
	for range 8 {
		closers.Add(1)
		go func() {
			defer closers.Done()
			p.closeFlow(flows, "client", be, &mu)
		}()
	}
	closers.Wait()
	if !waitFor(func() bool { return p.udpFlows.Load() == 0 && p.activity.count() == 0 }, time.Second) {
		t.Fatal("concurrent close failed to release exactly one slot")
	}
	replacement := start()
	p.closeFlow(flows, "client", be, &mu)
	if p.udpFlows.Load() != 1 || p.activity.count() != 1 {
		t.Error("late close released the replacement flow")
	}
	p.closeFlow(flows, "client", replacement, &mu)
	if !waitFor(func() bool { return p.udpFlows.Load() == 0 && p.activity.count() == 0 }, time.Second) {
		t.Fatal("replacement did not release its slot")
	}
}
