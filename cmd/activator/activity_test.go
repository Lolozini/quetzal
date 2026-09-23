package main

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// silentBackend accepts connections and holds them open without ever writing or
// closing: a game server with no read timeout on a client that says nothing.
func silentBackend(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
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
					_, _ = c.Write(buf[:n]) // echo only what it is sent
				}
			}(c)
		}
	}()
	return ln
}

// A connection that is open but carries nothing is not a player. Activity used
// to mean "a TCP flow exists", so one silent connection -- a scanner that never
// hangs up, a client that vanished without a FIN -- beat the idle timer every
// interval and the server could never hibernate again, for as long as the game
// itself tolerated the socket. UDP flows already expired after a minute of
// silence; TCP had no equivalent.
func TestSilentTCPConnectionIsNotActivity(t *testing.T) {
	be := silentBackend(t)
	var wakes, beats int32
	p := newTestProxy(&wakes, &beats)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go p.serveTCP(ln, be.Addr().String())

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Let the proxy connect through, then sit silent for many beat intervals
	// (the test proxy beats every 20ms).
	if !waitFor(func() bool { return p.activity.count() == 1 }, 2*time.Second) {
		t.Fatal("the flow never came up")
	}
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&beats); n != 0 {
		t.Fatalf("a silent connection reported activity %d times: it would keep the server awake forever", n)
	}

	// The same connection starts carrying traffic: that is a player.
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return atomic.LoadInt32(&beats) > 0 }, time.Second) {
		t.Fatal("traffic on the connection was not reported as activity")
	}

	// And going quiet again stops the heartbeat: nothing sticks.
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = c.Read(make([]byte, 16)) // drain the echo
	time.Sleep(60 * time.Millisecond)
	before := atomic.LoadInt32(&beats)
	time.Sleep(200 * time.Millisecond)
	if after := atomic.LoadInt32(&beats); after != before {
		t.Errorf("heartbeat kept going after the traffic stopped (%d -> %d)", before, after)
	}
}

// The fix must not cut anyone off: the silent connection is left open, it just
// no longer counts. A player in a game without keepalives who stops typing loses
// nothing unless the whole server goes idle for the full hibernation window.
func TestSilentTCPConnectionIsNotClosed(t *testing.T) {
	be := silentBackend(t)
	var wakes, beats int32
	p := newTestProxy(&wakes, &beats)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go p.serveTCP(ln, be.Addr().String())

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(300 * time.Millisecond)
	if _, err := c.Write([]byte("still here")); err != nil {
		t.Fatalf("the proxy dropped a quiet connection: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 32)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "still here" {
		t.Fatalf("quiet connection no longer relays: %q, %v", buf[:n], err)
	}
}
