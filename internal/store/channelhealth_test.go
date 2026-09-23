package store

import (
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func TestRecordChannelResult(t *testing.T) {
	s := newTestStore(t)
	c := &models.NotificationChannel{Name: "ops", Type: models.ChannelDiscord, Enabled: true}
	if err := s.CreateChannel(c, map[string]string{"url": "http://example.invalid"}); err != nil {
		t.Fatal(err)
	}
	created, _ := s.GetChannel(c.ID)
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

	// Two missed events in a row: the streak counts them and keeps the latest reason.
	if err := s.RecordChannelResult(c.ID, t0, "status 404"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChannelResult(c.ID, t0.Add(time.Minute), "status 503 (after 3 attempts)"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetChannel(c.ID)
	if got.FailureStreak != 2 {
		t.Errorf("streak = %d, want 2", got.FailureStreak)
	}
	if got.LastError != "status 503 (after 3 attempts)" || got.LastErrorAt == nil || !got.LastErrorAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("last error = %q at %v", got.LastError, got.LastErrorAt)
	}
	// Delivery traffic is not an edit: UpdatedAt says when somebody changed the channel.
	if !got.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("a delivery result moved UpdatedAt from %v to %v", created.UpdatedAt, got.UpdatedAt)
	}

	// A success clears the streak and stamps the delivery; the last error stays
	// on record so the panel can still say what went wrong before.
	if err := s.RecordChannelResult(c.ID, t0.Add(2*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetChannel(c.ID)
	if got.FailureStreak != 0 {
		t.Errorf("streak after a success = %d, want 0", got.FailureStreak)
	}
	if got.LastDeliveryAt == nil || !got.LastDeliveryAt.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("last delivery = %v", got.LastDeliveryAt)
	}

	// Editing the channel must not wipe its delivery record.
	_ = s.RecordChannelResult(c.ID, t0.Add(3*time.Minute), "boom")
	got.Name = "renamed"
	if err := s.UpdateChannel(got, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetChannel(c.ID)
	if got.FailureStreak != 1 || got.LastError != "boom" {
		t.Errorf("an edit clobbered the delivery record: streak=%d err=%q", got.FailureStreak, got.LastError)
	}
}

// A remote error body can be long; what is kept is bounded.
func TestRecordChannelResultBoundsTheMessage(t *testing.T) {
	s := newTestStore(t)
	c := &models.NotificationChannel{Name: "ops", Type: models.ChannelWebhook, Enabled: true}
	if err := s.CreateChannel(c, map[string]string{"url": "http://example.invalid"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChannelResult(c.ID, time.Now(), strings.Repeat("é", 2000)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetChannel(c.ID)
	if n := len([]rune(got.LastError)); n > 501 {
		t.Errorf("kept %d characters", n)
	}
}
