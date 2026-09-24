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
	"errors"
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
	gate := gateFromEnv()
	for _, p := range ports {
		ln, err := net.Listen("tcp", ":"+p)
		if err != nil {
			log.Fatalf("activator: listen %s: %v", p, err)
		}
		go dropListen(ln, p, w, gate)
	}
	log.Printf("activator(drop): waiting for %s to wake %q on %v", gate, slug, ports)
	select {}
}

// wakeGate is what the activator knows of the game's protocol, set by the
// controller from the template: which connections are a player.
type wakeGate struct {
	protocol string // "minecraft", or empty for any connection
	gamePort string // the port the game's own protocol is spoken on
}

func gateFromEnv() wakeGate {
	return wakeGate{protocol: os.Getenv("QUETZAL_WAKE_PROTOCOL"), gamePort: os.Getenv("QUETZAL_GAME_PORT")}
}

// minecraft reports whether only a Minecraft login may wake the server.
func (g wakeGate) minecraft() bool { return g.protocol == "minecraft" && g.gamePort != "" }

func (g wakeGate) String() string {
	if g.minecraft() {
		return "a Minecraft login on :" + g.gamePort
	}
	return "a connection"
}

const (
	// mcTimeout bounds a conversation with a client that has not joined: a
	// player's client sends its handshake at once.
	mcTimeout = 5 * time.Second
	// maxPending bounds the connections being read at once on a port, so a
	// flood costs a bounded amount; past it, connections are closed unread and
	// a player's client simply retries.
	maxPending = 64
)

func dropListen(ln net.Listener, port string, w *waker, gate wakeGate) {
	slots := make(chan struct{}, maxPending)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		switch {
		case !gate.minecraft():
			// Drop the connection first and wake in the background: the callback
			// can take seconds, and holding the accept loop meanwhile left every
			// other player's connection hanging behind it. trigger debounces.
			_ = conn.Close()
			go w.trigger()
		case port != gate.gamePort:
			// RCON, query and the like are not a player joining.
			_ = conn.Close()
		default:
			select {
			case slots <- struct{}{}:
				go func() {
					defer func() { <-slots }()
					dropMinecraft(conn, w)
				}()
			default:
				_ = conn.Close()
			}
		}
	}
}

// dropMinecraft reads a Minecraft client's handshake: a server-list ping gets
// the server shown as asleep, a player joining wakes it and is told to come
// back in a minute, and anything else is closed without waking anything.
func dropMinecraft(conn net.Conn, w *waker) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(mcTimeout))
	h, err := readMCHandshake(conn)
	if err != nil {
		return
	}
	if !h.joining() {
		_ = serveMCStatus(conn, h)
		return
	}
	name := readMCLoginName(conn)
	_ = mcDisconnect(conn, mcWakingReason)
	_ = conn.Close()
	log.Printf("activator: %s joining from %s wakes the server", name, conn.RemoteAddr())
	w.trigger()
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
