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
	body, _ := json.Marshal(discordEmbed(e))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doExpect2xx(client, req)
}

// discordEmbed renders an event as a Discord embed carrying the same fields as
// the activity log: the event type as the title, the message (which already
// includes the server) as the body, the actor and time as fields, and a colour
// keyed to severity so trouble stands out.
func discordEmbed(e models.Event) map[string]any {
	user := e.Username
	if user == "" {
		user = "system"
	}
	ts := e.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	embed := map[string]any{
		"title":     e.Type,
		"color":     discordColor(e.Type),
		"timestamp": ts.UTC().Format(time.RFC3339),
		"fields": []map[string]any{
			{"name": "User", "value": user, "inline": true},
		},
	}
	if e.Message != "" {
		embed["description"] = e.Message
	}
	return map[string]any{"embeds": []map[string]any{embed}}
}

// discordColor maps an event type to an embed colour: red for trouble, amber for
// a restart, green for healthy, grey for idle/stopped, blurple otherwise.
func discordColor(t string) int {
	switch t {
	case models.EventServerCrashed, models.EventServerOOMKilled:
		return 0xED4245 // red
	case models.EventServerRestarted:
		return 0xF0B232 // amber
	case models.EventServerRunning:
		return 0x57F287 // green
	case models.EventServerHibernated, models.EventServerStopped:
		return 0x99AAB5 // grey
	default:
		return 0x5865F2 // blurple
	}
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
	to := splitList(cfg["to"])
	if len(to) == 0 {
		return fmt.Errorf("email: to is required")
	}
	return SendMail(ctx, cfg, to, "[Quetzal] "+e.Type, summary(e))
}

// SendMail sends a plain-text email to the given recipients using the SMTP
// settings in cfg (host, port, username, password, from, tls). It is used both
// for notification email channels and for system mail such as password reset.
// net/smtp takes no context, so the whole conversation is bounded by a socket
// deadline derived from ctx.
func SendMail(ctx context.Context, cfg map[string]string, to []string, subject, body string) error {
	host := strings.TrimSpace(cfg["host"])
	from := strings.TrimSpace(cfg["from"])
	if host == "" || from == "" || len(to) == 0 {
		return fmt.Errorf("email: host, from and to are required")
	}
	port := cfg["port"]
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(host, port)
	mode := strings.ToLower(strings.TrimSpace(cfg["tls"]))

	msg := buildMessage(from, to, subject, body)

	var auth smtp.Auth
	if u := cfg["username"]; u != "" {
		auth = smtp.PlainAuth("", u, cfg["password"], host)
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// net/smtp takes no context, so bound the whole conversation with a socket
	// deadline. Without it a server that accepts the connection then stalls would
	// block the single dispatcher goroutine forever, wedging all notifications.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
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
