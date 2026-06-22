// Command activator fronts a hibernated server so a client connection can wake
// it. It has two modes (QUETZAL_MODE):
//
//   - "drop" (default): a lightweight TCP listener that, on the first
//     connection, asks the control plane to wake the server and then drops the
//     connection (clients reconnect once it is up). It is out of the data path
//     when the server is awake, so it adds no latency and the server sees the
//     real client IP.
//   - "proxy": an always-in-path TCP+UDP proxy to the real workload (via the
//     server's internal Service). It wakes the server on a new flow, holds/
//     forwards traffic transparently (no reconnect), supports UDP, and reports
//     activity so UDP servers can also auto-hibernate. Trade-offs: a small extra
//     hop and the server sees the proxy's IP rather than the client's.
//
// The activator holds no cluster credentials; it only nudges the database (the
// source of truth) via authenticated callbacks.
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
	switch os.Getenv("QUETZAL_MODE") {
	case "proxy":
		runProxy()
	default:
		runDrop()
	}
}

// runDrop is the lightweight wake-and-drop mode.
func runDrop() {
	wakeURL := os.Getenv("QUETZAL_WAKE_URL")
	ports := splitPorts(os.Getenv("QUETZAL_TCP_PORTS"))
	if wakeURL == "" || len(ports) == 0 {
		log.Fatalf("activator: QUETZAL_WAKE_URL and QUETZAL_TCP_PORTS are required")
	}
	slug := os.Getenv("QUETZAL_WAKE_SLUG")
	token := os.Getenv("QUETZAL_WAKE_TOKEN")
	w := &waker{cooldown: 15 * time.Second, post: func() error { return postCallback(wakeURL, slug, token) }}
	for _, p := range ports {
		go dropListen(p, w)
	}
	log.Printf("activator(drop): waiting for a connection to wake %q on %v", slug, ports)
	select {}
}

func dropListen(port string, w *waker) {
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

// waker fires a callback, debounced to at most once per cooldown so a burst of
// connection attempts produces a single call.
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
		// Don't let a failed wake suppress retries for the whole cooldown: clear
		// the timestamp so the next connection tries again immediately.
		w.mu.Lock()
		w.last = time.Time{}
		w.mu.Unlock()
	}
}

// postCallback POSTs {slug, token} to a control-plane callback URL.
func postCallback(url, slug, token string) error {
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
