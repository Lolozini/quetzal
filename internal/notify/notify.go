// Package notify delivers control-plane events to configured notification
// channels (Discord, generic webhooks, email). It drains a durable event outbox
// in the database: the apiserver runs one Dispatcher that, on a ticker or an
// explicit nudge, delivers every event past a persisted cursor to the channels
// that match it. Delivery is best-effort and at-least-once; a failing channel is
// logged and never blocks the others or stalls the cursor.
package notify

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/safefetch"
)

// cursorKey names the Setting row holding the last-delivered event ID.
const cursorKey = "notify.cursor"

// Store is the subset of the data store the dispatcher needs.
type Store interface {
	EnabledChannels() ([]models.NotificationChannel, error)
	ChannelConfig(*models.NotificationChannel) (map[string]string, error)
	EventsAfter(after uint, limit int) ([]models.Event, error)
	LatestEventID() (uint, error)
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
	// ServerIdentity resolves a server's display name and slug for labelling
	// notifications (both empty when the server is gone or id is 0).
	ServerIdentity(id uint) (name, slug string, err error)
}

// Dispatcher delivers events to channels.
type Dispatcher struct {
	Store    Store
	Interval time.Duration // poll cadence (a safety net behind nudges)
	Timeout  time.Duration // per-delivery timeout
	Batch    int           // max events drained per pass
	Client   *http.Client
	Logger   *log.Logger

	nudge chan struct{}
}

// New returns a dispatcher with homelab-sane defaults.
func New(st Store) *Dispatcher {
	return &Dispatcher{
		Store:    st,
		Interval: 15 * time.Second,
		Timeout:  10 * time.Second,
		Batch:    100,
		// Webhook/Discord URLs are user-supplied, so deliver through an
		// SSRF-guarded client: it refuses to connect to internal/private
		// addresses (loopback, RFC1918, link-local incl. cloud metadata) and
		// re-checks every redirect hop. External endpoints are unaffected.
		Client: &http.Client{
			Timeout:       10 * time.Second,
			Transport:     safefetch.SafeTransport(),
			CheckRedirect: safefetch.CheckRedirect,
		},
		Logger: log.Default(),
		nudge:  make(chan struct{}, 1),
	}
}

// Notify wakes the dispatcher for prompt delivery. Non-blocking and coalescing.
func (d *Dispatcher) Notify() {
	if d.nudge == nil {
		return
	}
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

// Run drains the outbox until ctx is cancelled. It seeds the cursor to the
// current latest event on first start so historical events are not replayed.
func (d *Dispatcher) Run(ctx context.Context) {
	if cur, _ := d.Store.GetSetting(cursorKey); cur == "" {
		if id, err := d.Store.LatestEventID(); err == nil {
			_ = d.Store.SetSetting(cursorKey, strconv.FormatUint(uint64(id), 10))
		}
	}
	t := time.NewTicker(d.Interval)
	defer t.Stop()
	for {
		d.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.nudge:
		}
	}
}

// drain delivers all pending events, advancing the cursor one event at a time so
// a crash re-delivers at most the in-flight event.
func (d *Dispatcher) drain(ctx context.Context) {
	cur := d.cursor()
	events, err := d.Store.EventsAfter(cur, d.Batch)
	if err != nil {
		d.Logger.Printf("notify: load events: %v", err)
		return
	}
	channels, err := d.Store.EnabledChannels()
	if err != nil {
		d.Logger.Printf("notify: load channels: %v", err)
		return
	}
	for _, e := range events {
		if ctx.Err() != nil {
			return
		}
		d.dispatch(ctx, e, channels)
		_ = d.Store.SetSetting(cursorKey, strconv.FormatUint(uint64(e.ID), 10))
	}
}

func (d *Dispatcher) cursor() uint {
	v, _ := d.Store.GetSetting(cursorKey)
	n, _ := strconv.ParseUint(v, 10, 64)
	return uint(n)
}

// dispatch delivers one event to every matching channel.
func (d *Dispatcher) dispatch(ctx context.Context, e models.Event, channels []models.NotificationChannel) {
	for i := range channels {
		c := channels[i]
		if !c.Matches(e.Type, e.ServerID) {
			continue
		}
		cfg, err := d.Store.ChannelConfig(&c)
		if err != nil {
			d.Logger.Printf("notify: channel %d config: %v", c.ID, err)
			continue
		}
		if err := d.DeliverTo(ctx, &c, cfg, e); err != nil {
			d.Logger.Printf("notify: channel %d (%s) deliver event %d: %v", c.ID, c.Type, e.ID, err)
		}
	}
}

// DeliverTo sends one event to a single channel. Exposed so the API can send a
// test event. It bounds the delivery with the dispatcher's timeout.
func (d *Dispatcher) DeliverTo(ctx context.Context, c *models.NotificationChannel, cfg map[string]string, e models.Event) error {
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	name, slug := d.serverIdentity(e.ServerID)
	switch c.Type {
	case models.ChannelDiscord:
		return deliverDiscord(ctx, d.Client, cfg, e, name, slug)
	case models.ChannelWebhook:
		return deliverWebhook(ctx, d.Client, cfg, e, name, slug)
	case models.ChannelEmail:
		return deliverEmail(ctx, cfg, e, name, slug)
	default:
		return errUnknownType(c.Type)
	}
}

// serverIdentity resolves a server's display name and slug for a notification,
// tolerating a missing store or a since-deleted server (returns empties).
func (d *Dispatcher) serverIdentity(id uint) (name, slug string) {
	if id == 0 || d.Store == nil {
		return "", ""
	}
	name, slug, err := d.Store.ServerIdentity(id)
	if err != nil {
		return "", ""
	}
	return name, slug
}
