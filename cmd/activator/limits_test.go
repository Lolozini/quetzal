package main

import (
	"net"
	"testing"
	"time"
)

// startEcho is a TCP backend that echoes and never hangs up first.
func startEcho(t *testing.T) net.Listener {
	t.Helper()
	be, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
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
					_, _ = c.Write(buf[:n])
				}
			}(c)
		}
	}()
	return be
}

func startFront(t *testing.T, p *proxy, backend string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go p.serveTCP(ln, backend, "")
	return ln.Addr().String()
}

// dialFrom connects to addr from a given loopback address, so one test can speak
// for several clients.
func dialFrom(t *testing.T, from, addr string) net.Conn {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}, Timeout: 2 * time.Second}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial from %s: %v", from, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// served says whether the proxy carries a connection: it echoes a byte. A turned
// away connection is closed by the proxy at once.
func served(c net.Conn) bool {
	if _, err := c.Write([]byte("x")); err != nil {
		return false
	}
	_ = c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 4)
	n, err := c.Read(buf)
	return err == nil && n == 1 && buf[0] == 'x'
}

// Nothing used to bound the connections the proxy held, for 64 MiB of pod: one
// client opening thousands of silent connections took it down, and with it the
// wake-up of every server behind it.
func TestOneSourceCannotFillTheTCPProxy(t *testing.T) {
	var wakes, beats int32
	p := newTestProxy(&wakes, &beats)
	p.tcpPerIPLimit = 2
	front := startFront(t, p, startEcho(t).Addr().String())

	a := dialFrom(t, "127.0.0.1", front)
	b := dialFrom(t, "127.0.0.1", front)
	if !served(a) || !served(b) {
		t.Fatal("a connection under the limit was not carried")
	}
	c := dialFrom(t, "127.0.0.1", front)
	if served(c) {
		t.Fatal("a third connection from one address was carried past its limit of 2")
	}
	// Another address is not affected by the first one's.
	d := dialFrom(t, "127.0.0.2", front)
	if !served(d) {
		t.Fatal("a second address was refused because of the first one's connections")
	}
	// The limit is on what is held: closing one gives its slot back.
	_ = a.Close()
	var again net.Conn
	for i := 0; i < 20; i++ { // the proxy notices the close a moment later
		again = dialFrom(t, "127.0.0.1", front)
		if served(again) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("a closed connection's slot was never given back")
}

func TestTheTCPProxyHasATotalCeiling(t *testing.T) {
	var wakes, beats int32
	p := newTestProxy(&wakes, &beats)
	p.tcpConnLimit, p.tcpPerIPLimit = 2, 100
	front := startFront(t, p, startEcho(t).Addr().String())

	if !served(dialFrom(t, "127.0.0.1", front)) || !served(dialFrom(t, "127.0.0.2", front)) {
		t.Fatal("a connection under the ceiling was not carried")
	}
	if served(dialFrom(t, "127.0.0.3", front)) {
		t.Fatal("a connection from a new address was carried past the ceiling of 2")
	}
}

// 128 UDP flows are shared by every source, and a flow is a source port: one
// address sending from 128 ports used to hold all of them.
func TestOneSourceCannotFillTheUDPFlows(t *testing.T) {
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
	p := &proxy{
		waker:         &waker{cooldown: time.Hour, post: func() error { return nil }},
		activity:      &activity{},
		udpPerIPLimit: 2,
	}
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	go p.serveUDP(front, be.LocalAddr().String())

	echo := func(from string, timeout time.Duration) bool {
		c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.ParseIP(from)}, front.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if _, err := c.Write([]byte("u")); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(timeout))
		buf := make([]byte, 8)
		n, err := c.Read(buf)
		return err == nil && string(buf[:n]) == "u"
	}
	for i := 0; i < 2; i++ {
		if !echo("127.0.0.1", 3*time.Second) {
			t.Fatalf("flow %d under the per-address limit was lost", i)
		}
	}
	if echo("127.0.0.1", 300*time.Millisecond) {
		t.Fatal("a third flow from one address was carried past its limit of 2")
	}
	if !echo("127.0.0.2", 3*time.Second) {
		t.Fatal("a second address was refused because of the first one's flows")
	}
	if got := p.activity.count(); got != 3 {
		t.Errorf("live flows = %d, want 3", got)
	}
}

func TestSourceCountIsExact(t *testing.T) {
	var c sourceCount
	if !c.acquire("a", 1) || c.acquire("a", 1) {
		t.Fatal("limit of 1 not enforced")
	}
	if !c.acquire("b", 1) {
		t.Fatal("limits are not per address")
	}
	c.release("a")
	if !c.acquire("a", 1) {
		t.Fatal("a released slot was not free")
	}
	c.release("a")
	c.release("a") // an extra release must not go negative or panic
	if !c.acquire("a", 1) || c.acquire("a", 1) {
		t.Fatal("an extra release corrupted the count")
	}
	if len(c.n) != 2 {
		t.Errorf("%d addresses tracked, want 2 (released ones are forgotten)", len(c.n))
	}
}
