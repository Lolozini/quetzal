package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func errUnknownType(t models.ChannelType) error {
	return permanent(fmt.Errorf("unknown channel type %q", t))
}

// statusError is a response outside 2xx. The code decides whether a retry can
// help; retryAfter is the server's own Retry-After, when it sent one.
type statusError struct {
	code       int
	retryAfter time.Duration
}

func (e *statusError) Error() string { return fmt.Sprintf("status %d", e.code) }

// permanentError marks a failure no retry can change: a setting that is
// missing, or a server that refuses what this configuration requires.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return permanentError{err} }

// parseRetryAfter reads a Retry-After header in either of its forms: a number
// of seconds (Discord sends a fraction), or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// serverLabel is the friendly server name for a notification: the display name,
// falling back to the slug (empty for panel-wide events).
func serverLabel(name, slug string) string {
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	return slug
}

// stripSlug drops the "slug: " prefix the event message carries, so the server
// can be shown as its own field/label instead of being duplicated inline.
func stripSlug(msg, slug string) string {
	if slug == "" {
		return msg
	}
	if p := slug + ": "; strings.HasPrefix(msg, p) {
		return msg[len(p):]
	}
	// An action with no detail records the slug alone, which carries nothing once
	// the server is its own field.
	if msg == slug {
		return ""
	}
	return msg
}

// eventUser is the actor for a notification, "system" for controller events.
func eventUser(e models.Event) string {
	if e.Username != "" {
		return e.Username
	}
	return "system"
}

// eventTime is the event's timestamp, defaulting to now when unset.
func eventTime(e models.Event) time.Time {
	if e.CreatedAt.IsZero() {
		return time.Now()
	}
	return e.CreatedAt
}

// ---- Discord ----

func deliverDiscord(ctx context.Context, client *http.Client, cfg map[string]string, e models.Event, name, slug string) error {
	url := strings.TrimSpace(cfg["url"])
	if url == "" {
		return permanent(fmt.Errorf("discord: missing url"))
	}
	body, _ := json.Marshal(discordEmbed(e, name, slug))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doExpect2xx(client, req)
}

// discordEmbed renders an event as a Discord embed carrying the same fields as
// the activity log: the event's title as the title, the message as the body, the
// server (its friendly name), the actor and the time as fields, and a colour
// keyed to severity so trouble stands out.
func discordEmbed(e models.Event, name, slug string) map[string]any {
	fields := make([]map[string]any, 0, 2)
	if label := serverLabel(name, slug); label != "" {
		fields = append(fields, map[string]any{"name": "Server", "value": label, "inline": true})
	}
	fields = append(fields, map[string]any{"name": "User", "value": eventUser(e), "inline": true})
	embed := map[string]any{
		"title":     models.EventTitle(e.Type),
		"color":     discordColor(e.Type),
		"timestamp": eventTime(e).UTC().Format(time.RFC3339),
		"fields":    fields,
		// The type itself, for whoever filters on it.
		"footer": map[string]any{"text": e.Type},
	}
	if msg := stripSlug(e.Message, slug); msg != "" {
		embed["description"] = msg
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
	ID         uint              `json:"id"`
	Type       string            `json:"type"`
	ServerID   uint              `json:"serverId,omitempty"`
	ServerName string            `json:"serverName,omitempty"`
	ServerSlug string            `json:"serverSlug,omitempty"`
	Username   string            `json:"username,omitempty"`
	Message    string            `json:"message"`
	Data       map[string]string `json:"data,omitempty"`
	Timestamp  string            `json:"timestamp"`
}

func deliverWebhook(ctx context.Context, client *http.Client, cfg map[string]string, e models.Event, name, slug string) error {
	url := strings.TrimSpace(cfg["url"])
	if url == "" {
		return permanent(fmt.Errorf("webhook: missing url"))
	}
	body, _ := json.Marshal(webhookPayload{
		ID: e.ID, Type: e.Type, ServerID: e.ServerID, ServerName: name, ServerSlug: slug,
		Username: e.Username, Message: e.Message, Data: e.Data,
		Timestamp: eventTime(e).UTC().Format(time.RFC3339),
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
		return &statusError{code: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	return nil
}

// ---- Email (SMTP) ----

func deliverEmail(ctx context.Context, cfg map[string]string, e models.Event, name, slug string) error {
	to := splitList(cfg["to"])
	if len(to) == 0 {
		return permanent(fmt.Errorf("email: to is required"))
	}
	label := serverLabel(name, slug)
	subject := "[Quetzal] "
	if label != "" {
		subject += label + " — "
	}
	subject += models.EventTitle(e.Type)
	return SendMail(ctx, cfg, to, subject, emailBody(e, label, slug))
}

// emailBody renders the same fields as the Discord embed as a plain-text block:
// server, event, actor, time, then the message.
func emailBody(e models.Event, label, slug string) string {
	var b strings.Builder
	if label != "" {
		fmt.Fprintf(&b, "Server: %s\n", label)
	}
	fmt.Fprintf(&b, "Event:  %s\n", e.Type)
	fmt.Fprintf(&b, "User:   %s\n", eventUser(e))
	fmt.Fprintf(&b, "Time:   %s\n", eventTime(e).UTC().Format(time.RFC3339))
	if msg := stripSlug(e.Message, slug); msg != "" {
		fmt.Fprintf(&b, "\n%s\n", msg)
	}
	return b.String()
}

// stripCRLF removes the line breaks that would end a header and let the rest of
// the value be read as further headers, or as the message body. Header values
// carry attacker-influenced text (a server's display name is a free label), so
// nothing reaches a header without passing through here.
func stripCRLF(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// encodeHeader makes an arbitrary string safe as a header value: no line breaks,
// and non-ASCII wrapped in an RFC 2047 encoded-word, since net/smtp never
// negotiates SMTPUTF8 and a raw 8-bit header is rejected by strict MTAs (and
// mangled by the rest). Pure ASCII is returned unchanged.
func encodeHeader(s string) string {
	return mime.QEncoding.Encode("utf-8", stripCRLF(s))
}

// ParseFrom reads a sender as people write one: an address, or a name and an
// address as "Quetzal <quetzal@example.com>". SMTP wants the address alone in
// its envelope (MAIL FROM): handed the whole string, the relay refused every
// message ("invalid FROM parameter"), password resets included, while the
// settings had been saved without a word.
func ParseFrom(from string) (*mail.Address, error) {
	a, err := mail.ParseAddress(strings.TrimSpace(from))
	if err != nil {
		return nil, fmt.Errorf("the sender %q is neither an email address nor a name and one, as Quetzal <quetzal@example.com>", from)
	}
	return a, nil
}

// greetingTimeout bounds the wait for the server's first words. A server that
// expects TLS from the first byte (SMTPS, usually port 465) waits in silence for
// the client to start it, while a client set to STARTTLS waits for the server's
// greeting: neither says anything, and the send ended at the deadline of the
// whole conversation, 20 seconds on, with "i/o timeout" and no hint of the
// cause. The greeting delays some relays impose on spammers stay well below.
var greetingTimeout = 10 * time.Second

// greetingError explains a conversation that failed before it began when the
// TLS mode is the likely cause, since neither way of getting it wrong looks
// like one. Anything else is returned as it came.
func greetingError(addr, mode string, err error) error {
	var ne net.Error
	var rh tls.RecordHeaderError
	switch {
	case mode != "tls" && errors.As(err, &ne) && ne.Timeout():
		// Not permanent: a relay that is merely slow to answer this once
		// deserves its retry.
		return fmt.Errorf(`email: %s said nothing within %s of the connection; a server that expects TLS from the first byte (usually port 465) waits like this, so choose "tls" if that is the case (%w)`,
			addr, greetingTimeout, err)
	case mode == "tls" && errors.As(err, &rh):
		return permanent(fmt.Errorf(`email: %s answered in plain text, so it does not expect TLS from the first byte; choose "starttls" (usually port 587) (%w)`, addr, err))
	}
	return err
}

// SendMail sends a plain-text email to the given recipients using the SMTP
// settings in cfg (host, port, username, password, from, tls). It is used both
// for notification email channels and for system mail such as password reset.
// net/smtp takes no context, so the whole conversation is bounded by a socket
// deadline derived from ctx.
func SendMail(ctx context.Context, cfg map[string]string, to []string, subject, body string) error {
	return Send(ctx, cfg, to, Mail{Subject: subject, Text: body})
}

// Mail is a message: its text, and optionally an HTML version of it with the
// images it shows. A client that does not display HTML shows the text.
type Mail struct {
	Subject string
	Text    string
	HTML    string
	// Inline are the HTML part's images, referenced from it as cid:<ID>. Sent
	// with the message rather than linked, so that they show without the
	// reader allowing remote images.
	Inline []Inline
}

// Inline is a file shown inside the HTML part.
type Inline struct {
	ID          string // Content-ID, without the angle brackets
	Name        string
	ContentType string
	Data        []byte
}

// Send delivers m through the configured relay; see SendMail.
func Send(ctx context.Context, cfg map[string]string, to []string, m Mail) error {
	host := strings.TrimSpace(cfg["host"])
	from := strings.TrimSpace(cfg["from"])
	if host == "" || from == "" || len(to) == 0 {
		return permanent(fmt.Errorf("email: host, from and to are required"))
	}
	sender, err := ParseFrom(from)
	if err != nil {
		return permanent(fmt.Errorf("email: %w", err))
	}
	// The header keeps the name, encoded if it is not ASCII; a bare address
	// stays as it was written.
	fromHeader := sender.Address
	if sender.Name != "" {
		fromHeader = sender.String()
	}
	port := cfg["port"]
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(host, port)
	mode := strings.ToLower(strings.TrimSpace(cfg["tls"]))

	msg, err := buildMail(fromHeader, to, m)
	if err != nil {
		return permanent(fmt.Errorf("email: %w", err))
	}

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
	dl, hasDeadline := ctx.Deadline()
	if hasDeadline {
		_ = conn.SetDeadline(dl)
	}
	// The greeting gets a shorter one of its own: see greetingTimeout.
	greet := time.Now().Add(greetingTimeout)
	if hasDeadline && dl.Before(greet) {
		greet = dl
	}
	_ = conn.SetReadDeadline(greet)
	// Implicit TLS (SMTPS, usually :465) wraps the connection immediately.
	if mode == "tls" {
		conn = tls.Client(conn, &tls.Config{ServerName: host})
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return greetingError(addr, mode, err)
	}
	defer client.Close()
	_ = conn.SetReadDeadline(dl) // the zero time when there is none: no deadline
	// STARTTLS is required, not attempted. This used to go ahead in cleartext
	// whenever the server did not offer it -- which is also what a man in the
	// middle arranges by deleting the offer from the EHLO reply. The password was
	// never at risk (net/smtp refuses PLAIN auth without TLS to anything but
	// localhost), but a relay that needs no auth then got the message itself in
	// the clear, and the messages this panel sends include password reset links.
	//
	// The form calls this mode "STARTTLS" and selects it by default, and an unset
	// mode is displayed the same way, so all of them mean it. Cleartext remains
	// available as "none", asked for by name.
	if mode != "tls" && mode != "none" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return permanent(fmt.Errorf("email: %s does not offer STARTTLS, which this configuration requires; nothing was sent. "+
				`Choose "tls" if the server expects TLS from the first byte (usually port 465), or "none" if it is a relay on a network you trust`, addr))
		}
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(sender.Address); err != nil {
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
	msg, _ := buildMail(from, to, Mail{Subject: subject, Text: body}) // text alone cannot fail
	return msg
}

// buildMail renders m as an RFC 5322 message. Text alone is sent as it always
// was. With HTML it becomes multipart/alternative — the text first, then the
// HTML with its images in a multipart/related — so that each client shows the
// richest part it can display.
func buildMail(from string, to []string, m Mail) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", stripCRLF(from))
	fmt.Fprintf(&b, "To: %s\r\n", stripCRLF(strings.Join(to, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", encodeHeader(m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	if m.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString("\r\n")
		b.WriteString(m.Text)
		b.WriteString("\r\n")
		return b.Bytes(), nil
	}

	alt := multipart.NewWriter(&b)
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", alt.Boundary())
	if err := writeQP(alt, "text/plain; charset=utf-8", m.Text); err != nil {
		return nil, err
	}
	var rel bytes.Buffer
	related := multipart.NewWriter(&rel)
	if err := writeQP(related, "text/html; charset=utf-8", m.HTML); err != nil {
		return nil, err
	}
	for _, f := range m.Inline {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", f.ContentType)
		h.Set("Content-Transfer-Encoding", "base64")
		h.Set("Content-ID", "<"+stripCRLF(f.ID)+">")
		h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": f.Name}))
		w, err := related.CreatePart(h)
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(f.Data)
		for len(enc) > 76 {
			io.WriteString(w, enc[:76]+"\r\n")
			enc = enc[76:]
		}
		io.WriteString(w, enc+"\r\n")
	}
	if err := related.Close(); err != nil {
		return nil, err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mime.FormatMediaType("multipart/related", map[string]string{"boundary": related.Boundary(), "type": "text/html"}))
	w, err := alt.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(rel.Bytes()); err != nil {
		return nil, err
	}
	if err := alt.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeQP adds a part in quoted-printable, which keeps its lines under the
// 998 characters SMTP allows whatever the content.
func writeQP(mw *multipart.Writer, contentType, body string) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	qp := quotedprintable.NewWriter(w)
	// Text mode: the writer turns each line break into CRLF itself.
	if _, err := io.WriteString(qp, body); err != nil {
		return err
	}
	return qp.Close()
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
