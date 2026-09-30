package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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
	EventServerCreate    = "server.create"
	EventServerDelete    = "server.delete"
	EventServerPower     = "server.power"
	EventServerUpdate    = "server.update"
	EventServerHibernate = "server.hibernation"
	EventServerRunning   = "server.running"   // controller: came up
	EventServerCrashed   = "server.crashed"   // controller: crashloop
	EventServerRestarted = "server.restarted" // controller: container restarted
	EventServerOOMKilled = "server.oomkilled" // controller: killed for OOM
	// EventServerInstallFailed is the install (or config-render) step exiting
	// non-zero. It is the failure an egg import produces most often — a dead
	// download URL, an apt mirror, a missing API key — and the one a user cannot
	// diagnose without being told.
	EventServerInstallFailed = "server.install-failed"
	EventServerStopped       = "server.stopped"    // controller: went down
	EventServerHibernated    = "server.hibernated" // controller: auto-slept
	EventServerTransfer      = "server.transfer"   // controller: cross-cluster move
	EventBackupCreate        = "backup.create"
	EventBackupRestore       = "backup.restore"
	// How a backup or a restore ended, scheduled or not. The two above are the
	// requests that start one, so a nightly backup failing used to be told to
	// no one.
	EventBackupSucceeded  = "backup.succeeded"
	EventBackupFailed     = "backup.failed"
	EventRestoreSucceeded = "restore.succeeded"
	EventRestoreFailed    = "restore.failed"
	EventScheduleCreate   = "schedule.create"
	EventScheduleDelete   = "schedule.delete"
	EventUserCreate       = "user.create"
	EventUserUpdate       = "user.update"
	EventUserDelete       = "user.delete"
	EventClusterCreate    = "cluster.create"
	EventClusterUpdate    = "cluster.update"
	EventClusterDelete    = "cluster.delete"
)

// EventTypes is every event a channel can filter on: the controller's
// lifecycle transitions and the panel's audited actions. A filter naming
// anything else never matches, and one on backup.succeeded, before that event
// existed, was accepted without a word and never received anything.
// TestEveryEmittedEventIsInTheCatalog keeps the list in step with the code.
var EventTypes = []string{
	// Seen by the controller.
	EventServerRunning, EventServerStopped, EventServerCrashed, EventServerRestarted,
	EventServerOOMKilled, EventServerInstallFailed, EventServerHibernated, EventServerTransfer,
	EventBackupSucceeded, EventBackupFailed, EventRestoreSucceeded, EventRestoreFailed,
	// Done through the panel.
	"server.create", "server.delete", "server.power", "server.update", "server.rename",
	"server.env", "server.resources", "server.image", "server.hibernation", "server.sftp",
	"server.reaches", "server.eula", "server.reinstall", "server.import", "server.suspend",
	"server.unsuspend", "server.wake",
	"backup.create", "backup.restore", "backup.delete", "backup.settings.update",
	"schedule.create", "schedule.update", "schedule.delete",
	"files.write", "files.delete", "files.rename", "files.move", "files.copy", "files.mkdir",
	"files.compress", "files.decompress", "files.extract",
	"database.create", "database.rotate", "database.delete",
	"dbhost.create", "dbhost.update", "dbhost.delete",
	"access.grant", "access.revoke",
	"user.create", "user.update", "user.delete", "user.email", "user.password",
	"user.password-reset", "user.adminrole",
	"2fa.enable", "2fa.disable", "2fa.admin-reset", "settings.require-2fa",
	"adminrole.create", "adminrole.update", "adminrole.delete",
	"apikey.create", "apikey.delete", "sshkey.add", "sshkey.delete",
	"cluster.create", "cluster.update", "cluster.delete",
	"template.import", "template.import-url", "template.update", "template.delete",
	"notification.create", "notification.update", "notification.delete",
	"email.settings.update", "email.settings.clear", "network.settings.update",
}

// eventTitles names the events people read about, in a mail's subject or a
// Discord embed's title. The others read from their type (EventTitle).
var eventTitles = map[string]string{
	EventServerRunning:       "Server is up",
	EventServerStopped:       "Server stopped",
	EventServerCrashed:       "Server crashed",
	EventServerRestarted:     "Server restarted",
	EventServerOOMKilled:     "Server ran out of memory",
	EventServerInstallFailed: "Install failed",
	EventServerHibernated:    "Server went to sleep",
	EventServerTransfer:      "Server transfer",
	EventBackupSucceeded:     "Backup done",
	EventBackupFailed:        "Backup failed",
	EventRestoreSucceeded:    "Restore done",
	EventRestoreFailed:       "Restore failed",
	"server.create":          "Server created",
	"server.delete":          "Server deleted",
	"server.power":           "Power action",
	"server.wake":            "Server woken",
	"backup.create":          "Backup started",
	"backup.restore":         "Restore started",
}

// EventTitle is an event type as a person reads it: "Server crashed", where
// mail subjects said server.crashed. A type without a title of its own reads
// from its words: "Notification create" for notification.create.
func EventTitle(t string) string {
	if title, ok := eventTitles[t]; ok {
		return title
	}
	words := strings.Join(strings.FieldsFunc(t, func(r rune) bool { return r == '.' || r == '-' }), " ")
	if words == "" {
		return t
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

// quietByDefault reports whether a channel receives an event of type t only
// when it asks for it. A channel's own changes are the panel's bookkeeping:
// creating one notified every other channel of it, which was noise.
func quietByDefault(t string) bool {
	return strings.HasPrefix(t, "notification.")
}

// KnownEventType reports whether t is one of EventTypes.
func KnownEventType(t string) bool {
	for _, k := range EventTypes {
		if k == t {
			return true
		}
	}
	return false
}

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

	// Events is the allow-list of event types this channel receives. Empty is
	// all of them but notification.*, which a channel gets when it lists them.
	Events EventList `json:"events"`

	// ConfigEnc is the encrypted JSON of the type-specific settings map. Never
	// serialized; the API exposes a masked view instead.
	ConfigEnc string `json:"-"`

	// Delivery health, written by the dispatcher and never by the API. A
	// delivery is tried a few times; one that still fails is not queued for
	// later -- the outbox cursor has moved on -- so FailureStreak is the number
	// of events this channel has missed since it last delivered one. Without
	// it, a channel pointing at a deleted webhook failed in the logs and nowhere
	// else, while looking perfectly healthy in the panel.
	FailureStreak  int        `json:"failureStreak"`
	LastError      string     `gorm:"size:512" json:"lastError,omitempty"`
	LastErrorAt    *time.Time `json:"lastErrorAt,omitempty"`
	LastDeliveryAt *time.Time `json:"lastDeliveryAt,omitempty"`
}

// EventList is a channel's event allow-list, stored as a JSON array. It carries
// its own Scanner/Valuer rather than relying on GORM's `serializer:json` tag,
// because that tag is only honoured on struct-shaped writes: a map-based
// Updates() call slips the raw Go slice past it and leaves a column no read can
// decode, which used to make one bad row fail every channel query — bricking
// notifications panel-wide and leaving the row undeletable through the API.
type EventList []string

// Value encodes the list as a JSON array; a nil list stores "[]" rather than
// NULL so the column always holds decodable JSON.
func (e EventList) Value() (driver.Value, error) {
	if e == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]string(e))
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan decodes the stored JSON array. A value that isn't decodable — written by
// an older build through the map-update path — degrades to the empty list,
// which the Matches contract already defines as "no filter". Losing a filter is
// recoverable (the channel shows up in the UI and can be re-saved); failing the
// scan is not, because it takes every other channel down with it.
func (e *EventList) Scan(v any) error {
	*e = nil
	var raw []byte
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		raw = t
	case string:
		raw = []byte(t)
	default:
		return fmt.Errorf("events: cannot scan %T", v)
	}
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil // salvage the row; treat as "no filter"
	}
	*e = out
	return nil
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
		return !quietByDefault(eventType)
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
