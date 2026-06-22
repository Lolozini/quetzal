package main

import (
	"net"
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
	go p.serveTCP(ln, be.Addr().String())

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
