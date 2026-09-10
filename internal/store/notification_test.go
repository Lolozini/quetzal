package store

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// TestUpdateChannelKeepsEventsReadable reproduces the production failure where
// updating a channel wrote its Events slice through a map-based Updates call,
// bypassing the JSON serializer and leaving a row that no read can decode.
func TestUpdateChannelKeepsEventsReadable(t *testing.T) {
	s := newTestStore(t)
	c := &models.NotificationChannel{
		Name: "probe", Type: models.ChannelWebhook, Enabled: true,
		Events: []string{"server.started"},
	}
	if err := s.CreateChannel(c, map[string]string{"url": "http://example.invalid"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ListChannels(); err != nil {
		t.Fatalf("list before update: %v", err)
	}
	c.Events = []string{"server.stopped"}
	if err := s.UpdateChannel(c, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.ListChannels()
	if err != nil {
		t.Fatalf("list AFTER update failed (row unreadable): %v", err)
	}
	if len(got) != 1 || len(got[0].Events) != 1 || got[0].Events[0] != "server.stopped" {
		t.Fatalf("events round-trip = %#v", got)
	}
	if _, err := s.GetChannel(c.ID); err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if _, err := s.EnabledChannels(); err != nil {
		t.Fatalf("EnabledChannels after update (dispatcher would go silent): %v", err)
	}
}

// TestCorruptedEventsColumnStaysReadable covers rows already written by the
// map-update path in production: one undecodable value must not fail every
// channel query, and the row must stay reachable so it can be fixed or deleted.
func TestCorruptedEventsColumnStaysReadable(t *testing.T) {
	s := newTestStore(t)
	good := &models.NotificationChannel{Name: "good", Type: models.ChannelWebhook, Enabled: true}
	if err := s.CreateChannel(good, map[string]string{"url": "http://example.invalid"}); err != nil {
		t.Fatalf("create good: %v", err)
	}
	bad := &models.NotificationChannel{Name: "bad", Type: models.ChannelWebhook, Enabled: true}
	if err := s.CreateChannel(bad, map[string]string{"url": "http://example.invalid"}); err != nil {
		t.Fatalf("create bad: %v", err)
	}
	// Exactly what the old map-based Updates() left behind: a bare Go string.
	if err := s.db.Exec("UPDATE notification_channels SET events = ? WHERE id = ?",
		"server.stopped", bad.ID).Error; err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	all, err := s.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels with one bad row: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListChannels = %d rows, want 2", len(all))
	}
	if _, err := s.EnabledChannels(); err != nil {
		t.Fatalf("EnabledChannels with one bad row (dispatcher goes silent): %v", err)
	}
	got, err := s.GetChannel(bad.ID)
	if err != nil {
		t.Fatalf("GetChannel on the bad row (blocks delete through the API): %v", err)
	}
	if len(got.Events) != 0 {
		t.Fatalf("salvaged events = %#v, want empty (no filter)", got.Events)
	}
	if err := s.DeleteChannel(bad.ID); err != nil {
		t.Fatalf("delete bad row: %v", err)
	}
}
