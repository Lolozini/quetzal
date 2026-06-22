package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func errUnknownType(t models.ChannelType) error {
	return fmt.Errorf("unknown channel type %q", t)
}

// summary renders a one-line human description of an event.
func summary(e models.Event) string {
	if e.Message != "" {
		return fmt.Sprintf("%s — %s", e.Type, e.Message)
	}
	return e.Type
}

// ---- Discord ----

func deliverDiscord(ctx context.Context, client *http.Client, cfg map[string]string, e models.Event) error {
	url := strings.TrimSpace(cfg["url"])
	if url == "" {
		return fmt.Errorf("discord: missing url")
	}
	body, _ := json.Marshal(map[string]string{"content": summary(e)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doExpect2xx(client, req)
}

// ---- Generic webhook ----

// webhookPayload is the stable JSON contract delivered to generic webhooks.
type webhookPayload struct {
	ID        uint              `json:"id"`
	Type      string            `json:"type"`
	ServerID  uint              `json:"serverId,omitempty"`
	Username  string            `json:"username,omitempty"`
	Message   string            `json:"message"`
	Data      map[string]string `json:"data,omitempty"`
	Timestamp string            `json:"timestamp"`
}

func deliverWebhook(ctx context.Context, client *http.Client, cfg map[string]string, e models.Event) error {
	url := strings.TrimSpace(cfg["url"])
	if url == "" {
		return fmt.Errorf("webhook: missing url")
	}
	ts := e.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	body, _ := json.Marshal(webhookPayload{
		ID: e.ID, Type: e.Type, ServerID: e.ServerID, Username: e.Username,
		Message: e.Message, Data: e.Data, Timestamp: ts.UTC().Format(time.RFC3339),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Quetzal-Event", e.Type)
	req.Header.Set("X-Quetzal-Delivery", strconv.FormatUint(uint64(e.ID), 10))
	if secret := cfg["secret"]; secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Quetzal-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return doExpect2xx(client, req)
}

func doExpect2xx(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// ---- Email (SMTP) ----

func deliverEmail(ctx context.Context, cfg map[string]string, e models.Event) error {
	host := strings.TrimSpace(cfg["host"])
	from := strings.TrimSpace(cfg["from"])
	to := splitList(cfg["to"])
	if host == "" || from == "" || len(to) == 0 {
		return fmt.Errorf("email: host, from and to are required")
	}
	port := cfg["port"]
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(host, port)
	mode := strings.ToLower(strings.TrimSpace(cfg["tls"]))

	subject := "[Quetzal] " + e.Type
	msg := buildMessage(from, to, subject, summary(e))

	var auth smtp.Auth
	if u := cfg["username"]; u != "" {
		auth = smtp.PlainAuth("", u, cfg["password"], host)
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// Implicit TLS (SMTPS, usually :465) wraps the connection immediately.
	if mode == "tls" {
		conn = tls.Client(conn, &tls.Config{ServerName: host})
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()
	// Opportunistic/explicit STARTTLS for non-implicit modes.
	if mode != "tls" && mode != "none" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return err
			}
		}
	}
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return err
		}
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(msg); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func buildMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
