package notify

import (
	"context"
	"mime"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// Mail subjects said the event's type, "[Quetzal] Terraria — server.power",
// or "notification.create". They say what happened now.
func TestAMailSubjectSaysWhatHappened(t *testing.T) {
	for _, c := range []struct{ typ, want string }{
		{"server.power", "[Quetzal] Terraria — Power action"},
		{models.EventServerCrashed, "[Quetzal] Terraria — Server crashed"},
		{"user.password-reset", "[Quetzal] Terraria — User password reset"},
	} {
		host, port, got := fakeSMTP(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := deliverEmail(ctx, map[string]string{"host": host, "port": port, "from": "q@example.test", "to": "ops@example.test", "tls": "none"},
			true, models.Event{Type: c.typ, Message: "terraria-a1b2: start"}, "Terraria", "terraria-a1b2")
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		subject := ""
		for _, l := range <-got {
			if raw, ok := strings.CutPrefix(l, "| Subject: "); ok {
				subject, _ = new(mime.WordDecoder).DecodeHeader(raw)
			}
		}
		if subject != c.want {
			t.Errorf("%s: subject %q, want %q", c.typ, subject, c.want)
		}
	}
}

// Creating a channel notified every other channel of it. A channel's own
// changes reach only a channel that asks for them.
func TestAChannelHearsOfOtherChannelsOnlyWhenItAsks(t *testing.T) {
	all := models.NotificationChannel{Enabled: true}
	asks := models.NotificationChannel{Enabled: true, Events: []string{"notification.create"}}
	if all.Matches("notification.create", 0) {
		t.Error("a channel with no filter heard of another channel's creation")
	}
	if !all.Matches(models.EventServerCrashed, 7) {
		t.Error("a channel with no filter missed a crash")
	}
	if !asks.Matches("notification.create", 0) {
		t.Error("a channel that asks for channel changes did not get them")
	}
}
