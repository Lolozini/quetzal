package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/notify"
	"github.com/lolozini/quetzal/internal/store"
)

type channelView struct {
	ID             uint       `json:"id"`
	FailureStreak  int        `json:"failureStreak"`
	LastError      string     `json:"lastError"`
	LastDeliveryAt *time.Time `json:"lastDeliveryAt"`
}

// A channel whose deliveries fail used to look healthy in the panel: the only
// trace was a log line. The miss is now on the channel, and the test button --
// the thing somebody presses after fixing the URL -- clears it.
func TestChannelHealthIsVisibleAndClearedByAGoodTest(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNotFound) // a webhook that has been deleted
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer recv.Close()

	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "h.db"), Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	apiSrv := api.New(st, fake.NewSimpleClientset(), &rest.Config{})
	d := notify.New(st)
	d.Interval = 20 * time.Millisecond
	d.Client = recv.Client() // permissive: loopback receiver
	apiSrv.Dispatch = d
	ts := httptest.NewServer(apiSrv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post(t, c, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, c, ts.URL+"/api/notifications/channels", map[string]any{
		"name": "hook", "type": "webhook", "enabled": true, "serverId": 0,
		"config": map[string]string{"url": recv.URL + "/hooks/SECRET-TOKEN"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create channel = %d", r.StatusCode)
	}
	decodeBody(t, r, &created)
	chURL := fmt.Sprintf("%s/api/notifications/channels/%d", ts.URL, created.ID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	for deadline := time.Now().Add(2 * time.Second); ; {
		if cur, _ := st.GetSetting(notify.CursorSetting); cur != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher never seeded its cursor")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A real event, delivered into a 404.
	post(t, c, ts.URL+"/api/apikeys", map[string]string{"name": "k"})
	var v channelView
	for deadline := time.Now().Add(3 * time.Second); ; {
		getJSON(t, c, chURL, &v)
		if v.FailureStreak > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failed delivery never showed on the channel")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if v.LastError != "status 404" {
		t.Errorf("lastError = %q, want %q", v.LastError, "status 404")
	}
	if strings.Contains(v.LastError, "SECRET-TOKEN") {
		t.Errorf("lastError exposes the masked URL: %q", v.LastError)
	}

	// A failed test is not an event the channel missed: the streak stays put.
	streak := v.FailureStreak
	if r := post(t, c, chURL+"/test", nil); r.StatusCode != http.StatusBadGateway {
		t.Fatalf("failing test = %d, want 502", r.StatusCode)
	}
	getJSON(t, c, chURL, &v)
	if v.FailureStreak != streak {
		t.Errorf("a failed test moved the streak from %d to %d", streak, v.FailureStreak)
	}

	// The URL is fixed; the test gets through and the channel is healthy again.
	status.Store(http.StatusNoContent)
	if r := post(t, c, chURL+"/test", nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("good test = %d", r.StatusCode)
	}
	getJSON(t, c, chURL, &v)
	if v.FailureStreak != 0 || v.LastDeliveryAt == nil {
		t.Errorf("after a good test: streak=%d lastDeliveryAt=%v, want 0 and set", v.FailureStreak, v.LastDeliveryAt)
	}
}

func decodeBody(t *testing.T, r *http.Response, v any) {
	t.Helper()
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
