package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

// shortWindow is the scan window shortScanWindow sets.
const shortWindow = 300 * time.Millisecond

// shortScanWindow makes a test's port scans and slow clients quick to tell apart.
func shortScanWindow(t *testing.T) {
	t.Helper()
	old := scanWindow.Swap(int64(shortWindow))
	t.Cleanup(func() { scanWindow.Store(old) })
}

// Outside Minecraft, a port scanner's bare connection woke a sleeping server
// (measured on Terraria: one connection, awake 15 s later), so a server on the
// internet never stayed asleep. A connection that hangs up without a byte wakes
// nothing now; one that speaks, or waits for the server to, still does.
func TestDropIgnoresPortScans(t *testing.T) {
	shortScanWindow(t)
	ln, port := listen(t)
	w, wakes := countingWaker()
	go dropListen(ln, port, w, wakeGate{})
	addr := ln.Addr().String()

	c := dial(t, addr)
	_ = c.Close()
	time.Sleep(3 * shortWindow)
	if n := wakes.Load(); n != 0 {
		t.Fatalf("a bare connection woke the server (%d)", n)
	}

	// A client that speaks first: most games.
	c = dial(t, addr)
	_, _ = c.Write([]byte{0x0f, 0x00, 0x01, 'T', 'e', 'r', 'r', 'a', 'r', 'i', 'a'})
	eventually(t, func() bool { return wakes.Load() == 1 })
	_ = c.Close()

	// A client that waits for the server to speak first.
	c = dial(t, addr)
	eventually(t, func() bool { return wakes.Load() == 2 })
	_ = c.Close()
}

// The same in proxy mode, where the client's first bytes must still reach the
// server once it is up.
func TestProxyIgnoresPortScansWhileAsleep(t *testing.T) {
	shortScanWindow(t)
	ln, port := listen(t)
	w, wakes := countingWaker()
	backend := closedAddr(t)
	p := &proxy{waker: w, activity: &activity{}, dialBudget: 5 * time.Second}
	go p.serveTCP(ln, backend, port)

	c := dial(t, ln.Addr().String())
	_ = c.Close()
	time.Sleep(backendDial + 3*shortWindow)
	if n := wakes.Load(); n != 0 {
		t.Fatalf("a bare connection woke the server (%d)", n)
	}

	c = dial(t, ln.Addr().String())
	defer c.Close()
	_, _ = c.Write([]byte("hello"))
	eventually(t, func() bool { return wakes.Load() == 1 })
	// The server comes up where the proxy expects it.
	up, err := net.Listen("tcp", backend)
	if err != nil {
		t.Skipf("could not take the backend's port back: %v", err)
	}
	defer up.Close()
	_ = up.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	s, err := up.Accept()
	if err != nil {
		t.Fatalf("the woken client never reached the server: %v", err)
	}
	defer s.Close()
	_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, _ := s.Read(buf)
	if string(buf[:n]) != "hello" {
		t.Errorf("the server got %q, want the client's first bytes", buf[:n])
	}
}

func raknetPingBytes(tm uint64) []byte {
	b := []byte{raknetPing}
	b = binary.BigEndian.AppendUint64(b, tm)
	b = append(b, raknetMagic...)
	return binary.BigEndian.AppendUint64(b, 42)
}

func raknetJoinBytes() []byte {
	b := append([]byte{raknetOpenRequest1}, raknetMagic...)
	return append(b, 11, 0, 0, 0)
}

func raknetPongBytes(tm uint64, line string) []byte {
	b := []byte{raknetPong}
	b = binary.BigEndian.AppendUint64(b, tm)
	b = binary.BigEndian.AppendUint64(b, 0xabcdef)
	b = append(b, raknetMagic...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(line)))
	return append(b, line...)
}

// udpProxy serves a proxy on a local UDP port and returns a client socket.
func udpProxy(t *testing.T, p *proxy, backend string) *net.UDPConn {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("front: %v", err)
	}
	t.Cleanup(func() { front.Close() })
	go p.serveUDP(front, backend)
	c, err := net.DialUDP("udp", nil, front.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// closedUDPAddr is a UDP address nothing listens on: a sleeping server.
func closedUDPAddr(t *testing.T) string {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

// Any UDP packet woke a sleeping server: a Bedrock client refreshing its server
// list (measured: one ping, awake 15 s later), a tracker's Steam query. A query
// wakes nothing now; the first packet that is not one still does.
func TestUDPQueriesDoNotWake(t *testing.T) {
	w, wakes := countingWaker()
	p := &proxy{waker: w, activity: &activity{}}
	c := udpProxy(t, p, closedUDPAddr(t))

	for _, q := range [][]byte{
		raknetPingBytes(1),
		append([]byte{0xff, 0xff, 0xff, 0xff}, "TSource Engine Query\x00"...),
		{0xfe, 0xfd, 0x09, 0, 0, 0, 1},
		[]byte("M-SEARCH * HTTP/1.1\r\n\r\n"),
	} {
		_, _ = c.Write(q)
	}
	time.Sleep(200 * time.Millisecond)
	if n := wakes.Load(); n != 0 {
		t.Fatalf("queries woke the server (%d)", n)
	}
	if p.activity.seen.Load() {
		t.Error("queries counted as activity")
	}

	_, _ = c.Write([]byte{0x05, 0x13, 0x37})
	eventually(t, func() bool { return wakes.Load() == 1 })
}

// On a Minecraft Java server, UDP was the query port and nothing on it woke the
// server, so a Bedrock player coming through Geyser could not. Opening a RakNet
// connection is a player joining, whatever the server.
func TestGeyserPlayerWakesAJavaServer(t *testing.T) {
	w, wakes := countingWaker()
	p := &proxy{waker: w, activity: &activity{}, gate: wakeGate{protocol: "minecraft", gamePort: "25565"}}
	c := udpProxy(t, p, closedUDPAddr(t))

	_, _ = c.Write(raknetPingBytes(1))
	_, _ = c.Write([]byte{0xfe, 0xfd, 0x09, 0, 0, 0, 1}) // Java query
	time.Sleep(200 * time.Millisecond)
	if n := wakes.Load(); n != 0 {
		t.Fatalf("a ping or a query woke the server (%d)", n)
	}
	_, _ = c.Write(raknetJoinBytes())
	eventually(t, func() bool { return wakes.Load() == 1 })
}

// A sleeping Bedrock server did not answer its server list, so players saw it
// offline. The activator now answers with the server's own last line, shown
// asleep -- never one it made up, which could carry a version the client does
// not expect.
func TestBedrockListShowsTheServerAsleep(t *testing.T) {
	const line = "MCPE;A real world;712;1.21.40;3;10;123456;Bedrock level;Survival;1;19132;19133;"
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	baddr := backend.LocalAddr().String()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := backend.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if classifyUDP(buf[:n]) == packetPing {
				_, _ = backend.WriteToUDP(raknetPongBytes(binary.BigEndian.Uint64(buf[1:9]), line), from)
			}
		}
	}()

	read := func(c *net.UDPConn) []byte {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1500)
		n, err := c.Read(buf)
		if err != nil {
			return nil
		}
		return buf[:n]
	}

	// Before the server has answered once, a sleeping one is not answered.
	w, wakes := countingWaker()
	fresh := &proxy{waker: w, activity: &activity{}}
	c := udpProxy(t, fresh, closedUDPAddr(t))
	_, _ = c.Write(raknetPingBytes(7))
	if got := read(c); got != nil {
		t.Errorf("a line made up with nothing to go on: %q", got)
	}

	// Awake, the server answers for itself.
	p := &proxy{waker: w, activity: &activity{}}
	c = udpProxy(t, p, baddr)
	_, _ = c.Write(raknetPingBytes(7))
	if got := read(c); !bytes.Contains(got, []byte("A real world")) {
		t.Fatalf("the awake server's pong did not come through: %q", got)
	}
	// Asleep, the activator answers in its place.
	_ = backend.Close()
	time.Sleep(100 * time.Millisecond)
	_, _ = c.Write(raknetPingBytes(9))
	got := read(c)
	if got == nil {
		t.Fatal("a sleeping server's list was not answered")
	}
	if binary.BigEndian.Uint64(got[1:9]) != 9 {
		t.Errorf("the ping's time was not echoed")
	}
	want := "MCPE;" + mcAsleepMOTD + ";712;1.21.40;0;10;123456;Bedrock level;Survival;1;19132;19133;"
	if s := string(got[raknetPongHeaderLen:]); s != want {
		t.Errorf("asleep line %q, want %q", s, want)
	}
	if n := wakes.Load(); n != 0 {
		t.Errorf("pings woke the server (%d)", n)
	}
}

func TestClassifyUDP(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want packetKind
	}{
		{"RakNet ping", raknetPingBytes(1), packetPing},
		{"RakNet open connection", raknetJoinBytes(), packetJoin},
		{"A2S_INFO", append([]byte{0xff, 0xff, 0xff, 0xff}, "TSource Engine Query\x00"...), packetQuery},
		{"Source connect challenge", append([]byte{0xff, 0xff, 0xff, 0xff}, "q"...), packetOther},
		{"GoldSrc getchallenge", append([]byte{0xff, 0xff, 0xff, 0xff}, "getchallenge steam"...), packetOther},
		{"GameSpy query", []byte{0xfe, 0xfd, 0x09, 0, 0, 0, 1}, packetQuery},
		{"a game's own packet", []byte{0x05, 0x13, 0x37}, packetOther},
		{"short", []byte{0x01}, packetOther},
		{"SIP scanner", []byte("OPTIONS sip:100@1.2.3.4 SIP/2.0\r\n"), packetQuery},
	}
	for _, c := range cases {
		if got := classifyUDP(c.b); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if !strings.Contains(mcAsleepMOTD, "wake") || strings.Contains(mcAsleepMOTD, ";") {
		t.Errorf("the asleep MOTD %q cannot go in a Bedrock line", mcAsleepMOTD)
	}
}
