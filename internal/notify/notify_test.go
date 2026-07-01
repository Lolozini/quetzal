package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// fakeStore implements notify.Store in memory.
type fakeStore struct {
	mu       sync.Mutex
	events   []models.Event
	channels []models.NotificationChannel
	settings map[string]string
}

func (f *fakeStore) EnabledChannels() ([]models.NotificationChannel, error) {
	var out []models.NotificationChannel
	for _, c := range f.channels {
		if c.Enabled {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeStore) ChannelConfig(c *models.NotificationChannel) (map[string]string, error) {
	return map[string]string{"url": c.ConfigEnc, "secret": "s3cr3t"}, nil
}
func (f *fakeStore) EventsAfter(after uint, limit int) ([]models.Event, error) {
	var out []models.Event
	for _, e := range f.events {
		if e.ID > after {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeStore) LatestEventID() (uint, error) {
	var max uint
	for _, e := range f.events {
		if e.ID > max {
			max = e.ID
		}
	}
	return max, nil
}
func (f *fakeStore) GetSetting(k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings[k], nil
}
func (f *fakeStore) SetSetting(k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settings == nil {
		f.settings = map[string]string{}
	}
	f.settings[k] = v
	return nil
}

func TestDispatcherMatchesAndAdvances(t *testing.T) {
	var got []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer srv.Close()

	st := &fakeStore{
		events: []models.Event{
			{ID: 1, Type: models.EventServerCrashed, ServerID: 7, Message: "boom"},
			{ID: 2, Type: models.EventServerPower, ServerID: 9, Message: "ignored"},
			{ID: 3, Type: models.EventUserCreate, Message: "panel"},
		},
		channels: []models.NotificationChannel{
			// Scoped to server 7, only crashes -> matches event 1 only.
			{ID: 1, Type: models.ChannelDiscord, Enabled: true, ServerID: 7,
				Events: []string{models.EventServerCrashed}, ConfigEnc: srv.URL},
			// Disabled -> never fires.
			{ID: 2, Type: models.ChannelDiscord, Enabled: false, ConfigEnc: srv.URL},
		},
		settings: map[string]string{cursorKey: "0"}, // explicit cursor: no seeding skip
	}
	d := New(st)
	d.Client = srv.Client() // permissive: this test targets a loopback receiver
	d.drain(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 delivery, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "boom") {
		t.Errorf("payload missing message: %s", got[0])
	}
	if cur := st.settings[cursorKey]; cur != "3" {
		t.Errorf("cursor = %q, want 3 (advances past all events)", cur)
	}
}

func TestDispatcherSeedsCursorOnFirstRun(t *testing.T) {
	st := &fakeStore{
		events:   []models.Event{{ID: 5, Type: models.EventServerPower}},
		settings: map[string]string{}, // no cursor yet
	}
	d := New(st)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	// Give Run a moment to seed, then stop.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, _ := st.GetSetting(cursorKey); v != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if v, _ := st.GetSetting(cursorKey); v != "5" {
		t.Errorf("first-run cursor = %q, want 5 (no replay of history)", v)
	}
}

// TestDispatcherClientBlocksSSRF locks in that the dispatcher's default client
// refuses webhook delivery to an internal/loopback address (SSRF guard), so a
// user-supplied channel URL can't reach in-cluster or metadata endpoints.
func TestDispatcherClientBlocksSSRF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))
	defer srv.Close()

	d := New(&fakeStore{}) // default (guarded) client, not srv.Client()
	c := &models.NotificationChannel{Type: models.ChannelWebhook}
	err := d.DeliverTo(context.Background(), c, map[string]string{"url": srv.URL}, models.Event{ID: 1})
	if err == nil {
		t.Fatal("expected delivery to a loopback address to be refused")
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Errorf("error = %v, want SSRF guard refusal", err)
	}
}

func TestWebhookSignsAndSetsHeaders(t *testing.T) {
	const secret = "s3cr3t"
	var gotSig, gotEvent string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Quetzal-Signature")
		gotEvent = r.Header.Get("X-Quetzal-Event")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	e := models.Event{ID: 42, Type: models.EventServerCrashed, ServerID: 3, Message: "down"}
	err := deliverWebhook(context.Background(), srv.Client(), map[string]string{"url": srv.URL, "secret": secret}, e)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if gotEvent != models.EventServerCrashed {
		t.Errorf("X-Quetzal-Event = %q", gotEvent)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("signature = %q, want %q", gotSig, want)
	}
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if p.ID != 42 || p.ServerID != 3 || p.Message != "down" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestWebhookNoSecretNoSignature(t *testing.T) {
	var hasSig bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasSig = r.Header["X-Quetzal-Signature"]
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := deliverWebhook(context.Background(), srv.Client(), map[string]string{"url": srv.URL}, models.Event{ID: 1}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if hasSig {
		t.Error("unsigned webhook must not set a signature header")
	}
}

func TestDiscordSendsContent(t *testing.T) {
	var payload struct {
		Embeds []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Color       int    `json:"color"`
			Fields      []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"fields"`
		} `json:"embeds"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &payload)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	e := models.Event{Type: models.EventServerRunning, Message: "srv: is up"}
	if err := deliverDiscord(context.Background(), srv.Client(), map[string]string{"url": srv.URL}, e); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("want one embed, got %+v", payload)
	}
	em := payload.Embeds[0]
	if em.Title != models.EventServerRunning {
		t.Errorf("embed title = %q, want %q", em.Title, models.EventServerRunning)
	}
	if !strings.Contains(em.Description, "is up") {
		t.Errorf("embed description = %q", em.Description)
	}
	// A controller event has no actor, so the User field falls back to "system".
	var user string
	for _, f := range em.Fields {
		if f.Name == "User" {
			user = f.Value
		}
	}
	if user != "system" {
		t.Errorf("User field = %q, want system", user)
	}
}

func TestNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	if err := deliverDiscord(context.Background(), srv.Client(), map[string]string{"url": srv.URL}, models.Event{}); err == nil {
		t.Error("expected error on 500")
	}
}

func TestEmailValidatesAndBuildsMessage(t *testing.T) {
	// Missing required fields -> error, no dial attempted.
	if err := deliverEmail(context.Background(), map[string]string{"host": "mail"}, models.Event{}); err == nil {
		t.Error("expected error when from/to missing")
	}
	msg := string(buildMessage("a@x.test", []string{"b@y.test", "c@y.test"}, "Subj", "Body"))
	for _, want := range []string{"From: a@x.test", "To: b@y.test, c@y.test", "Subject: Subj", "\r\n\r\nBody"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q in:\n%s", want, msg)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a@x , b@x; c@x\n")
	if len(got) != 3 || got[0] != "a@x" || got[2] != "c@x" {
		t.Errorf("splitList = %v", got)
	}
}
