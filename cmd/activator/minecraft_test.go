package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mcHandshakeBytes is what a Minecraft client sends first.
func mcHandshakeBytes(protocol int32, addr string, port uint16, next int32) []byte {
	body := appendVarInt(nil, protocol)
	body = appendString(body, addr)
	body = binary.BigEndian.AppendUint16(body, port)
	body = appendVarInt(body, next)
	var b bytes.Buffer
	_ = writePacket(&b, 0, body)
	return b.Bytes()
}

func mcLoginStart(name string) []byte {
	var b bytes.Buffer
	body := appendString(nil, name)
	body = append(body, make([]byte, 16)...) // the player's UUID
	_ = writePacket(&b, 0, body)
	return b.Bytes()
}

func TestReadMCHandshake(t *testing.T) {
	status := mcHandshakeBytes(767, "play.example.com", 25565, mcStateStatus)
	h, err := readMCHandshake(bytes.NewReader(status))
	if err != nil {
		t.Fatalf("status handshake: %v", err)
	}
	if h.protocol != 767 || h.address != "play.example.com" || h.port != 25565 || h.joining() {
		t.Errorf("parsed %s, joining=%v", h, h.joining())
	}
	if !bytes.Equal(h.raw, status) {
		t.Error("the raw packet must be kept whole, to replay to the server")
	}
	for _, next := range []int32{mcStateLogin, mcStateTransfer} {
		h, err := readMCHandshake(bytes.NewReader(mcHandshakeBytes(767, "x", 25565, next)))
		if err != nil || !h.joining() {
			t.Errorf("state %d: %v, joining=%v; want a player joining", next, err, h != nil && h.joining())
		}
	}

	// What scanners and stray clients send is not a handshake.
	oversized := appendVarInt(nil, mcMaxHandshake+1)
	trailing := append([]byte(nil), status...)
	trailing[0]++ // one more byte than the fields use
	trailing = append(trailing, 0)
	for name, in := range map[string][]byte{
		"nothing":       {},
		"an HTTP probe": []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		"a TLS hello":   {0x16, 0x03, 0x01, 0x00, 0xa5, 0x01, 0x00},
		"a legacy ping": {0xfe, 0x01, 0xfa},
		"a huge length": oversized,
		"another state": mcHandshakeBytes(767, "x", 25565, 4),
		"extra bytes":   trailing,
	} {
		if _, err := readMCHandshake(bytes.NewReader(in)); err == nil {
			t.Errorf("%s was taken for a handshake", name)
		}
	}
}

// countingWaker counts wakes.
func countingWaker() (*waker, *atomic.Int32) {
	var n atomic.Int32
	return &waker{post: func() error { n.Add(1); return nil }}, &n
}

func listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return ln, port
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return c
}

// pingStatus runs a server-list ping and returns the MOTD and whether the pong
// came back.
func pingStatus(t *testing.T, c net.Conn) (motd string, pong bool) {
	t.Helper()
	_, _ = c.Write(mcHandshakeBytes(767, "x", 25565, mcStateStatus))
	var req bytes.Buffer
	_ = writePacket(&req, 0, nil)
	_, _ = c.Write(req.Bytes())
	id, body, _, err := readPacket(c, 1<<16)
	if err != nil || id != 0 {
		t.Fatalf("status response: id %d, %v", id, err)
	}
	js, err := readString(bytes.NewReader(body), 1<<16)
	if err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	var st struct {
		Version     struct{ Protocol int32 }
		Description struct{ Text string }
	}
	if err := json.Unmarshal([]byte(js), &st); err != nil {
		t.Fatalf("status JSON %q: %v", js, err)
	}
	if st.Version.Protocol != 767 {
		t.Errorf("the status names protocol %d, want the client's own", st.Version.Protocol)
	}
	var ping bytes.Buffer
	_ = writePacket(&ping, 1, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	_, _ = c.Write(ping.Bytes())
	id, body, _, err = readPacket(c, 64)
	return st.Description.Text, err == nil && id == 1 && bytes.Equal(body, []byte{1, 2, 3, 4, 5, 6, 7, 8})
}

// joinAndReadReason logs in and returns the disconnect message.
func joinAndReadReason(t *testing.T, c net.Conn) string {
	t.Helper()
	_, _ = c.Write(mcHandshakeBytes(767, "x", 25565, mcStateLogin))
	_, _ = c.Write(mcLoginStart("Steve"))
	id, body, _, err := readPacket(c, 1<<16)
	if err != nil || id != 0 {
		t.Fatalf("login disconnect: id %d, %v", id, err)
	}
	js, _ := readString(bytes.NewReader(body), 1<<16)
	var reason struct{ Text string }
	_ = json.Unmarshal([]byte(js), &reason)
	return reason.Text
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// A sleeping Minecraft server used to be woken by any TCP connection, so every
// scanner on the internet kept it cycling awake. Only a player joining wakes it
// now; the server list shows it asleep.
func TestDropWakesOnlyOnAMinecraftLogin(t *testing.T) {
	ln, port := listen(t)
	w, wakes := countingWaker()
	go dropListen(ln, port, w, wakeGate{protocol: "minecraft", gamePort: port})
	addr := ln.Addr().String()

	// A bare connect, an HTTP probe and a legacy ping wake nothing.
	c := dial(t, addr)
	_ = c.Close()
	c = dial(t, addr)
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\n\r\n")
	_, _ = io.ReadAll(c) // closed by the activator
	_ = c.Close()
	c = dial(t, addr)
	_, _ = c.Write([]byte{0xfe, 0x01, 0xfa})
	_, _ = io.ReadAll(c)
	_ = c.Close()

	// A server-list ping is answered, and wakes nothing.
	c = dial(t, addr)
	motd, pong := pingStatus(t, c)
	_ = c.Close()
	if motd != mcAsleepMOTD || !pong {
		t.Errorf("status: motd %q, pong %v", motd, pong)
	}
	time.Sleep(50 * time.Millisecond)
	if n := wakes.Load(); n != 0 {
		t.Fatalf("%d wakes before any player joined", n)
	}

	// A player joining wakes it, and is told to come back.
	c = dial(t, addr)
	if reason := joinAndReadReason(t, c); reason != mcWakingReason {
		t.Errorf("disconnect reason %q", reason)
	}
	_ = c.Close()
	eventually(t, func() bool { return wakes.Load() == 1 })
}

// On a Minecraft server, the other ports (RCON, query) are not a player.
func TestDropIgnoresTheOtherPortsOfAMinecraftServer(t *testing.T) {
	ln, port := listen(t)
	w, wakes := countingWaker()
	go dropListen(ln, port, w, wakeGate{protocol: "minecraft", gamePort: "25565"})
	c := dial(t, ln.Addr().String())
	_, _ = c.Write(mcHandshakeBytes(767, "x", 25565, mcStateLogin))
	_, _ = io.ReadAll(c)
	_ = c.Close()
	time.Sleep(50 * time.Millisecond)
	if n := wakes.Load(); n != 0 {
		t.Errorf("%d wakes from the RCON port", n)
	}
}

// Other games keep waking on any connection.
func TestDropWakesOnAnyConnectionForOtherGames(t *testing.T) {
	ln, port := listen(t)
	w, wakes := countingWaker()
	go dropListen(ln, port, w, wakeGate{})
	c := dial(t, ln.Addr().String())
	_ = c.Close()
	eventually(t, func() bool { return wakes.Load() == 1 })
}

// mcBackend is a stand-in game server that records what reaches it.
func mcBackend(t *testing.T) (addr string, got chan []byte) {
	t.Helper()
	ln, _ := listen(t)
	got = make(chan []byte, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				var buf bytes.Buffer
				_, err := io.Copy(&buf, c)
				var ne net.Error
				if err == nil || errors.As(err, &ne) {
					got <- buf.Bytes()
				}
			}()
		}
	}()
	return ln.Addr().String(), got
}

// closedAddr is an address nothing listens on: a sleeping server.
func closedAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestProxyWhileTheServerSleeps(t *testing.T) {
	ln, port := listen(t)
	w, wakes := countingWaker()
	act := &activity{}
	p := &proxy{waker: w, activity: act, gate: wakeGate{protocol: "minecraft", gamePort: port}}
	go p.serveTCP(ln, closedAddr(t), port)

	c := dial(t, ln.Addr().String())
	motd, _ := pingStatus(t, c)
	_ = c.Close()
	if motd != mcAsleepMOTD {
		t.Errorf("a sleeping server's status: %q", motd)
	}
	if wakes.Load() != 0 {
		t.Fatal("a server-list ping woke the server")
	}

	c = dial(t, ln.Addr().String())
	if reason := joinAndReadReason(t, c); reason != mcWakingReason {
		t.Errorf("disconnect reason %q", reason)
	}
	_ = c.Close()
	if wakes.Load() != 1 {
		t.Errorf("%d wakes, want the player's one", wakes.Load())
	}
}

func TestProxyWhileTheServerIsUp(t *testing.T) {
	backend, got := mcBackend(t)
	ln, port := listen(t)
	w, wakes := countingWaker()
	act := &activity{}
	p := &proxy{waker: w, activity: act, gate: wakeGate{protocol: "minecraft", gamePort: port}}
	go p.serveTCP(ln, backend, port)

	// A server-list ping reaches the server whole, but is not activity: a
	// scanner polling the list must not keep the server awake.
	status := mcHandshakeBytes(767, "x", 25565, mcStateStatus)
	c := dial(t, ln.Addr().String())
	_, _ = c.Write(append(status, 0x01, 0x00))
	_ = c.(*net.TCPConn).CloseWrite()
	if b := <-got; !bytes.Equal(b, append(status, 0x01, 0x00)) {
		t.Errorf("the server received %x, want the ping as sent", b)
	}
	_ = c.Close()
	if act.seen.Load() || wakes.Load() != 0 {
		t.Errorf("a server-list ping counted: activity %v, wakes %d", act.seen.Load(), wakes.Load())
	}

	// A player's traffic reaches it and is activity.
	login := append(mcHandshakeBytes(767, "x", 25565, mcStateLogin), mcLoginStart("Steve")...)
	c = dial(t, ln.Addr().String())
	_, _ = c.Write(login)
	_ = c.(*net.TCPConn).CloseWrite()
	if b := <-got; !bytes.Equal(b, login) {
		t.Errorf("the server received %x, want the login as sent", b)
	}
	_ = c.Close()
	if !act.seen.Load() {
		t.Error("a player's traffic did not count as activity")
	}
}

// The status must be valid JSON whatever the MOTD holds.
func TestStatusIsJSON(t *testing.T) {
	var out bytes.Buffer
	in := bytes.NewReader([]byte{0x01, 0x00})
	h := &mcHandshake{protocol: 5}
	_ = serveMCStatus(struct {
		io.Reader
		io.Writer
	}{in, &out}, h)
	_, body, _, err := readPacket(&out, 1<<16)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	js, _ := readString(bytes.NewReader(body), 1<<16)
	if !json.Valid([]byte(js)) || !strings.Contains(js, `"protocol":5`) {
		t.Errorf("status %q", js)
	}
}
