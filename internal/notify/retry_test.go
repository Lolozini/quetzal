package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// scripted answers each request with the next status in its script, repeating
// the last one once the script runs out.
type scripted struct {
	mu      sync.Mutex
	script  []int
	header  http.Header
	hits    int32
	byEvent map[string]int
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	i := int(atomic.AddInt32(&s.hits, 1)) - 1
	code := s.script[len(s.script)-1]
	if i < len(s.script) {
		code = s.script[i]
	}
	if s.byEvent == nil {
		s.byEvent = map[string]int{}
	}
	s.byEvent[r.Header.Get("X-Quetzal-Delivery")]++
	for k, v := range s.header {
		w.Header()[k] = v
	}
	s.mu.Unlock()
	w.WriteHeader(code)
}

// newRetryDispatcher returns a dispatcher over st whose backoff is recorded
// instead of slept.
func newRetryDispatcher(st Store, client *http.Client) (*Dispatcher, *[]time.Duration) {
	d := New(st)
	d.Client = client
	var waits []time.Duration
	var mu sync.Mutex
	d.sleep = func(ctx context.Context, dur time.Duration) error {
		mu.Lock()
		waits = append(waits, dur)
		mu.Unlock()
		return ctx.Err()
	}
	return d, &waits
}

func webhookStore(url string, events ...uint) *fakeStore {
	st := &fakeStore{
		channels: []models.NotificationChannel{{ID: 1, Type: models.ChannelWebhook, Enabled: true, ConfigEnc: url}},
		settings: map[string]string{CursorSetting: "0"},
	}
	for _, id := range events {
		st.events = append(st.events, models.Event{ID: id, Type: models.EventServerCrashed, Message: "boom"})
	}
	return st
}

// A blip passes: two 503s and then the endpoint is back, and the event arrives.
// It used to be dropped on the first 503, with a log line as the only trace.
func TestDeliveryRetriesATransientFailure(t *testing.T) {
	h := &scripted{script: []int{503, 503, 204}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	st := webhookStore(srv.URL, 1)
	d, waits := newRetryDispatcher(st, srv.Client())

	d.drain(context.Background())

	if h.hits != 3 {
		t.Fatalf("requests = %d, want 3", h.hits)
	}
	if len(st.results) != 1 || st.results[0].errMsg != "" {
		t.Fatalf("results = %+v, want one success", st.results)
	}
	// Backoff doubles between attempts.
	if len(*waits) != 2 || (*waits)[0] != 2*time.Second || (*waits)[1] != 4*time.Second {
		t.Errorf("waits = %v, want [2s 4s]", *waits)
	}
}

// A refusal that will not change is not retried: a 404 from a deleted webhook
// would cost every event three requests and the backoff between them, for
// nothing.
func TestDeliveryDoesNotRetryAPermanentRefusal(t *testing.T) {
	h := &scripted{script: []int{404}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	st := webhookStore(srv.URL, 1)
	d, waits := newRetryDispatcher(st, srv.Client())

	d.drain(context.Background())

	if h.hits != 1 {
		t.Errorf("requests = %d, want 1", h.hits)
	}
	if len(*waits) != 0 {
		t.Errorf("backed off before giving up on a 404: %v", *waits)
	}
	if len(st.results) != 1 || st.results[0].errMsg != "status 404" {
		t.Errorf("results = %+v", st.results)
	}
}

// A channel that stays down past its attempts misses the event -- and says so
// on the channel, which is what the panel shows. The cursor still moves on:
// one dead channel must not hold up the outbox.
func TestExhaustedDeliveryIsRecordedAndTheCursorMovesOn(t *testing.T) {
	h := &scripted{script: []int{503}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	st := webhookStore(srv.URL, 1)
	d, _ := newRetryDispatcher(st, srv.Client())

	d.drain(context.Background())

	if h.hits != 3 {
		t.Errorf("requests = %d, want 3", h.hits)
	}
	if len(st.results) != 1 || st.results[0].errMsg != "status 503 (after 3 attempts)" {
		t.Errorf("results = %+v", st.results)
	}
	if cur, _ := st.GetSetting(CursorSetting); cur != "1" {
		t.Errorf("cursor = %q, want 1: a failing channel stalled the outbox", cur)
	}
}

// Once a channel has exhausted its attempts in a pass, its later events in the
// same pass are recorded as missed without trying: a channel that hangs until
// the timeout would otherwise cost every event in the batch its full retry
// sequence, with every other channel waiting behind it. A healthy channel
// beside it still gets everything.
func TestAFailingChannelIsNotRetriedForTheRestOfThePass(t *testing.T) {
	bad := &scripted{script: []int{503}}
	badSrv := httptest.NewServer(bad)
	defer badSrv.Close()
	good := &scripted{script: []int{204}}
	goodSrv := httptest.NewServer(good)
	defer goodSrv.Close()

	st := webhookStore(badSrv.URL, 1, 2, 3)
	st.channels = append(st.channels, models.NotificationChannel{ID: 2, Type: models.ChannelWebhook, Enabled: true, ConfigEnc: goodSrv.URL})
	d, _ := newRetryDispatcher(st, http.DefaultClient)
	d.Client = &http.Client{Timeout: 5 * time.Second}

	d.drain(context.Background())

	if bad.hits != 3 {
		t.Errorf("failing channel got %d requests for 3 events, want 3 (one retry sequence)", bad.hits)
	}
	if good.hits != 3 {
		t.Errorf("healthy channel got %d of 3 events", good.hits)
	}
	var missed int
	for _, r := range st.results {
		if r.channel == 1 && r.errMsg != "" {
			missed++
			if r != st.results[0] && !strings.Contains(r.errMsg, "not attempted") {
				t.Errorf("skipped event recorded as %q", r.errMsg)
			}
		}
	}
	if missed != 3 {
		t.Errorf("missed events recorded for the failing channel = %d, want 3 (the streak must count every one)", missed)
	}
}

// A server that says when to come back is listened to, within reason: while
// the dispatcher waits, every other channel waits with it.
func TestDeliveryHonoursRetryAfterWithinACap(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"1.5", 1500 * time.Millisecond}, // Discord sends fractions of a second
		{"3600", maxRetryAfter},
	} {
		h := &scripted{script: []int{429, 204}, header: http.Header{"Retry-After": {tc.header}}}
		srv := httptest.NewServer(h)
		st := webhookStore(srv.URL, 1)
		d, waits := newRetryDispatcher(st, srv.Client())
		d.drain(context.Background())
		srv.Close()
		if len(*waits) != 1 || (*waits)[0] != tc.want {
			t.Errorf("Retry-After %s: waited %v, want [%v]", tc.header, *waits, tc.want)
		}
		if len(st.results) != 1 || st.results[0].errMsg != "" {
			t.Errorf("Retry-After %s: results = %+v", tc.header, st.results)
		}
	}
}

// The URL of a Discord or webhook channel is its secret -- the API masks it
// everywhere -- and net/http puts it in every transport error. What the panel
// shows must not carry it.
func TestRecordedErrorDoesNotLeakTheChannelURL(t *testing.T) {
	const secretURL = "http://127.0.0.1:1/api/webhooks/123/SUPER-SECRET-TOKEN"
	st := webhookStore(secretURL, 1)
	d, _ := newRetryDispatcher(st, &http.Client{Timeout: time.Second})

	d.drain(context.Background())

	if len(st.results) != 1 || st.results[0].errMsg == "" {
		t.Fatalf("results = %+v, want one failure", st.results)
	}
	if msg := st.results[0].errMsg; strings.Contains(msg, "SUPER-SECRET-TOKEN") || strings.Contains(msg, "/api/webhooks") {
		t.Errorf("recorded error leaks the URL: %q", msg)
	}
	// The test button's error goes back over the API, so it is held to the same rule.
	err := d.DeliverTo(context.Background(), &st.channels[0], map[string]string{"url": secretURL}, models.Event{Type: "test"})
	if err == nil || strings.Contains(err.Error(), "SUPER-SECRET-TOKEN") {
		t.Errorf("test delivery error leaks the URL: %v", err)
	}
}

// Shutting down mid-backoff stops at once and is not blamed on the channel.
func TestShutdownDuringBackoffIsNotAChannelFailure(t *testing.T) {
	h := &scripted{script: []int{503}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	st := webhookStore(srv.URL, 1)
	d := New(st)
	d.Client = srv.Client()
	ctx, cancel := context.WithCancel(context.Background())
	d.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }

	d.drain(ctx)

	if h.hits != 1 {
		t.Errorf("requests = %d, want 1", h.hits)
	}
	if len(st.results) != 0 {
		t.Errorf("a shutdown was recorded against the channel: %+v", st.results)
	}
	// And the event is still ahead of the cursor, so the restart delivers it.
	if cur, _ := st.GetSetting(CursorSetting); cur != "0" {
		t.Errorf("cursor = %q after an interrupted delivery, want 0: the event is lost", cur)
	}
}

// A URL the SSRF guard refuses is refused identically every time; retrying it
// only holds every other channel up for the length of the backoff.
func TestDeliveryDoesNotRetryAnAddressTheGuardRefuses(t *testing.T) {
	st := webhookStore("http://127.0.0.1:1/api/webhooks/9/TOKEN", 1)
	d := New(st) // the production client, SSRF guard included
	var waits []time.Duration
	d.sleep = func(_ context.Context, dur time.Duration) error { waits = append(waits, dur); return nil }

	d.drain(context.Background())

	if len(waits) != 0 {
		t.Errorf("backed off %v before giving up on a refused address", waits)
	}
	if len(st.results) != 1 || !strings.Contains(st.results[0].errMsg, "non-public address") {
		t.Fatalf("results = %+v", st.results)
	}
	if strings.Contains(st.results[0].errMsg, "TOKEN") {
		t.Errorf("recorded error leaks the URL: %q", st.results[0].errMsg)
	}
}
