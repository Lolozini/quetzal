package models

import "time"

// ChannelType identifies a notification sink implementation.
type ChannelType string

const (
	ChannelDiscord ChannelType = "discord" // Discord incoming webhook
	ChannelWebhook ChannelType = "webhook" // generic HMAC-signed JSON POST
	ChannelEmail   ChannelType = "email"   // SMTP
)

// Event types. These mirror the audited actions plus controller-observed
// lifecycle transitions, and are the values channels filter on.
const (
	EventServerCreate     = "server.create"
	EventServerDelete     = "server.delete"
	EventServerPower      = "server.power"
	EventServerUpdate     = "server.update"
	EventServerHibernate  = "server.hibernation"
	EventServerRunning    = "server.running"    // controller: came up
	EventServerCrashed    = "server.crashed"    // controller: crashloop
	EventServerRestarted  = "server.restarted"  // controller: container restarted
	EventServerOOMKilled  = "server.oomkilled"  // controller: killed for OOM
	EventServerStopped    = "server.stopped"    // controller: went down
	EventServerHibernated = "server.hibernated" // controller: auto-slept
	EventServerTransfer   = "server.transfer"   // controller: cross-cluster move
	EventBackupCreate     = "backup.create"
	EventBackupRestore    = "backup.restore"
	EventScheduleCreate   = "schedule.create"
	EventScheduleDelete   = "schedule.delete"
	EventUserCreate       = "user.create"
	EventUserUpdate       = "user.update"
	EventUserDelete       = "user.delete"
	EventClusterCreate    = "cluster.create"
	EventClusterUpdate    = "cluster.update"
	EventClusterDelete    = "cluster.delete"
)

// NotificationChannel is a configured outbound sink. Its type-specific settings
// (webhook URLs, signing secrets, SMTP credentials) are encrypted at rest; the
// DB only ever holds ciphertext in ConfigEnc.
type NotificationChannel struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	Name    string      `json:"name"`
	Type    ChannelType `gorm:"size:32" json:"type"`
	Enabled bool        `json:"enabled"`

	// ServerID scopes the channel: 0 = global (receives every event, panel and
	// server); >0 = only that server's events. A server's deletion cascades to
	// its channels.
	ServerID uint `gorm:"index" json:"serverId"`

	// Events is the allow-list of event types this channel receives. Empty = all.
	Events []string `gorm:"serializer:json" json:"events"`

	// ConfigEnc is the encrypted JSON of the type-specific settings map. Never
	// serialized; the API exposes a masked view instead.
	ConfigEnc string `json:"-"`
}

// Config keys per channel type (stored encrypted in ConfigEnc):
//
//	discord: url
//	webhook: url, secret
//	email:   host, port, username, password, from, to, tls ("starttls"|"tls"|"none")
//
// SecretConfigKeys lists, per type, the keys that must be masked in API
// responses (their presence is reported, never their value).
var SecretConfigKeys = map[ChannelType][]string{
	ChannelDiscord: {"url"},
	ChannelWebhook: {"url", "secret"},
	ChannelEmail:   {"password"},
}

// Matches reports whether the channel should receive an event of the given type
// for the given server. ServerID 0 is a global catch-all.
func (c NotificationChannel) Matches(eventType string, serverID uint) bool {
	if !c.Enabled {
		return false
	}
	if c.ServerID != 0 && c.ServerID != serverID {
		return false
	}
	if len(c.Events) == 0 {
		return true
	}
	for _, e := range c.Events {
		if e == eventType {
			return true
		}
	}
	return false
}

// Event is an occurrence worth notifying about. It doubles as a durable outbox:
// the apiserver's dispatcher delivers events with ID greater than a stored
// cursor to every matching channel. Both the apiserver (user actions) and the
// controller (lifecycle transitions) append events.
type Event struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`

	Type     string `gorm:"index;size:64" json:"type"`
	ServerID uint   `gorm:"index" json:"serverId,omitempty"` // 0 = panel-wide
	UserID   uint   `json:"userId,omitempty"`                // actor; 0 = system/controller
	Username string `json:"username,omitempty"`
	Message  string `json:"message"`

	Data map[string]string `gorm:"serializer:json" json:"data,omitempty"`
}

// Setting is a small key/value row for control-plane state that has no natural
// home in a typed table (e.g. the notification delivery cursor).
type Setting struct {
	Key   string `gorm:"primaryKey;size:128" json:"key"`
	Value string `json:"value"`
}
