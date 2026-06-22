// Command activator stands in for a hibernated server. It listens on the
// server's TCP game ports and, on the first connection, asks the control plane
// to wake the server (scale it back up) via an authenticated callback. It holds
// no cluster credentials and no game state — it only nudges the database (the
// source of truth); the controller then scales the real workload and repoints
// the Service. The triggering connection is dropped, so clients reconnect once
// the server is up. TCP only; wake-and-proxy and UDP are intentionally out of
// scope (see README).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	wakeURL := os.Getenv("QUETZAL_WAKE_URL")
	ports := splitPorts(os.Getenv("QUETZAL_PORTS"))
	if wakeURL == "" || len(ports) == 0 {
		log.Fatalf("activator: QUETZAL_WAKE_URL and QUETZAL_PORTS are required")
	}
	slug := os.Getenv("QUETZAL_WAKE_SLUG")
	token := os.Getenv("QUETZAL_WAKE_TOKEN")
	w := &waker{
		cooldown: 15 * time.Second,
		post:     func() error { return postWake(wakeURL, slug, token) },
	}
	for _, p := range ports {
		go listen(p, w, slug)
	}
	log.Printf("activator: waiting for a connection to wake %q on ports %v", slug, ports)
	select {} // run until the pod is removed
}

// waker fires a wake callback, debounced to at most once per cooldown so a burst
// of connection attempts produces a single wake.
type waker struct {
	cooldown time.Duration
	post     func() error
	now      func() time.Time
	mu       sync.Mutex
	last     time.Time
}

func (w *waker) trigger() {
	now := time.Now
	if w.now != nil {
		now = w.now
	}
	w.mu.Lock()
	t := now()
	if !w.last.IsZero() && t.Sub(w.last) < w.cooldown {
		w.mu.Unlock()
		return
	}
	w.last = t
	w.mu.Unlock()
	if err := w.post(); err != nil {
		log.Printf("activator: wake callback failed: %v", err)
	}
}

func listen(port string, w *waker, slug string) {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("activator: listen %s: %v", port, err)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		w.trigger()
		_ = conn.Close()
	}
}

func postWake(url, slug, token string) error {
	body, _ := json.Marshal(map[string]string{"slug": slug, "token": token})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func splitPorts(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
