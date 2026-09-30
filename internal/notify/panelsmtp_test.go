package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// An email channel needed an SMTP server of its own, typed in again although
// the panel already sends its password resets through one. A channel that
// names no host sends through the panel's.
func TestAnEmailChannelSendsThroughThePanelsServer(t *testing.T) {
	host, port, got := fakeSMTP(t)
	st := &fakeStore{smtp: map[string]string{"host": host, "port": port, "from": "Quetzal <panel@example.test>", "tls": "none"}}
	d := New(st)
	c := &models.NotificationChannel{ID: 1, Type: models.ChannelEmail, Enabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := d.DeliverTo(ctx, c, map[string]string{"to": "ops@example.test"}, models.Event{Type: models.EventServerCrashed, Message: "crashed"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	lines := <-got
	from := false
	for _, l := range lines {
		from = from || strings.HasPrefix(l, "MAIL FROM:<panel@example.test>")
	}
	if !from || !has(lines, "RCPT TO:<ops@example.test>") {
		t.Errorf("the mail did not go out through the panel's server as its sender: %s", strings.Join(lines, " / "))
	}

	// With no server of its own and none on the panel, it says so.
	st.smtp = nil
	err = d.DeliverTo(ctx, c, map[string]string{"to": "ops@example.test"}, models.Event{Type: models.EventServerCrashed})
	if err == nil || !strings.Contains(err.Error(), "no SMTP server") {
		t.Errorf("no server anywhere = %v, want an error saying so", err)
	}
}
