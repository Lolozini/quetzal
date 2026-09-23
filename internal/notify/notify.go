// Package notify delivers control-plane events to configured notification
// channels (Discord, generic webhooks, email). It drains a durable event outbox
// in the database: the apiserver runs one Dispatcher that, on a ticker or an
// explicit nudge, delivers every event past a persisted cursor to the channels
// that match it.
//
// Each delivery gets a few attempts with backoff, for the failures that pass on
// their own (a 5xx, a rate limit, a dropped connection); a refusal that will
// not change (a 404 from a deleted webhook, a bad address) is not retried. The
// cursor then moves on regardless: one dead channel must not hold up the rest.
// So a channel that stays down past its attempts misses the event -- delivery
// is at-least-once only for the event in flight across a restart -- and that
// is recorded on the channel (FailureStreak, LastError) where the panel shows
// it, rather than in a log line nobody reads.
package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/safefetch"
)

// CursorSetting names the Setting row holding the last-delivered event ID.
// CursorSetting is the settings key holding the dispatcher's position in the
// event outbox. Exported so a test can tell whether Run has seeded it: an event
// created before that seeding is skipped for good, not delivered late.
const CursorSetting = "notify.cursor"

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
	// RecordChannelResult notes how a delivery ended: errMsg empty for success.
	RecordChannelResult(id uint, at time.Time, errMsg string) error
}

// Dispatcher delivers events to channels.
type Dispatcher struct {
	Store    Store
	Interval time.Duration // poll cadence (a safety net behind nudges)
	Timeout  time.Duration // per-delivery timeout
	Batch    int           // max events drained per pass
	Client   *http.Client
	Logger   *log.Logger
	// Attempts is how many times a delivery is tried before the event counts as
	// missed for that channel; Backoff is the wait before the second attempt,
	// doubling after that.
	Attempts int
	Backoff  time.Duration

	nudge chan struct{}
	// sleep waits between attempts; replaced in tests so they do not wait.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

// maxRetryAfter caps how long a server's Retry-After can hold the dispatcher.
// Every other channel waits behind this one while it sleeps.
const maxRetryAfter = 10 * time.Second

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
		Logger:   log.Default(),
		Attempts: 3,
		Backoff:  2 * time.Second,
		nudge:    make(chan struct{}, 1),
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
	if cur, _ := d.Store.GetSetting(CursorSetting); cur == "" {
		if id, err := d.Store.LatestEventID(); err == nil {
			_ = d.Store.SetSetting(CursorSetting, strconv.FormatUint(uint64(id), 10))
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
	// A channel that exhausts its attempts in this pass is not tried again until
	// the next one: its remaining events here are recorded as missed straight
	// away. Without that, a channel that hangs until the timeout would cost every
	// event in the batch its full retry sequence, and hold every other channel up
	// behind it for as long.
	tripped := map[uint]string{}
	for _, e := range events {
		if ctx.Err() != nil {
			return
		}
		d.dispatch(ctx, e, channels, tripped)
		if ctx.Err() != nil {
			// Interrupted mid-event: leave the cursor behind it, so the restart
			// delivers it again. A channel that already had it gets it twice,
			// which is the at-least-once the package promises.
			return
		}
		_ = d.Store.SetSetting(CursorSetting, strconv.FormatUint(uint64(e.ID), 10))
	}
}

func (d *Dispatcher) cursor() uint {
	v, _ := d.Store.GetSetting(CursorSetting)
	// bitSize 0, so a value that would not fit a uint is rejected rather than
	// wrapped: a wrapped cursor points backwards and replays old events.
	n, _ := strconv.ParseUint(v, 10, 0)
	return uint(n)
}

// dispatch delivers one event to every matching channel. The event's server is
// resolved once here rather than per channel, so fanning out to several sinks
// costs one lookup, not one each.
func (d *Dispatcher) dispatch(ctx context.Context, e models.Event, channels []models.NotificationChannel, tripped map[uint]string) {
	var name, slug string
	var resolved bool
	for i := range channels {
		c := channels[i]
		if !c.Matches(e.Type, e.ServerID) {
			continue
		}
		if reason, ok := tripped[c.ID]; ok {
			d.record(c.ID, e.ID, "not attempted, the channel was failing: "+reason)
			continue
		}
		if !resolved {
			name, slug = d.serverIdentity(e.ServerID)
			resolved = true
		}
		cfg, err := d.Store.ChannelConfig(&c)
		if err != nil {
			d.record(c.ID, e.ID, "could not read the channel's settings")
			d.Logger.Printf("notify: channel %d config: %v", c.ID, err)
			continue
		}
		if err := d.deliverWithRetry(ctx, &c, cfg, e, name, slug); err != nil {
			if ctx.Err() != nil {
				return // shutting down: this is not the channel's failure
			}
			msg := describe(err)
			tripped[c.ID] = msg
			d.record(c.ID, e.ID, msg)
			continue
		}
		d.record(c.ID, e.ID, "")
	}
}

// deliverWithRetry makes up to Attempts deliveries, backing off between them,
// and stops early on a failure that another try would not change.
func (d *Dispatcher) deliverWithRetry(ctx context.Context, c *models.NotificationChannel, cfg map[string]string, e models.Event, name, slug string) error {
	attempts := d.Attempts
	if attempts < 1 {
		attempts = 1
	}
	wait := d.Backoff
	var err error
	for i := 1; ; i++ {
		err = d.deliverTo(ctx, c, cfg, e, name, slug)
		if err == nil {
			return nil
		}
		if i >= attempts || !retryable(err) {
			if i > 1 {
				return fmt.Errorf("%w (after %d attempts)", err, i)
			}
			return err
		}
		pause := wait
		var se *statusError
		if errors.As(err, &se) && se.retryAfter > 0 {
			pause = se.retryAfter
		}
		if pause > maxRetryAfter {
			pause = maxRetryAfter
		}
		if serr := d.pause(ctx, pause); serr != nil {
			return serr
		}
		wait *= 2
	}
}

// record stores a delivery's outcome on its channel and, for a miss, logs it.
func (d *Dispatcher) record(channelID, eventID uint, errMsg string) {
	if errMsg != "" {
		d.Logger.Printf("notify: channel %d missed event %d: %s", channelID, eventID, errMsg)
	}
	if err := d.Store.RecordChannelResult(channelID, d.clock(), errMsg); err != nil {
		d.Logger.Printf("notify: channel %d: record delivery result: %v", channelID, err)
	}
}

func (d *Dispatcher) pause(ctx context.Context, dur time.Duration) error {
	if d.sleep != nil {
		return d.sleep(ctx, dur)
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Dispatcher) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// retryable reports whether trying again could plausibly succeed.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var pe permanentError
	if errors.As(err, &pe) {
		return false
	}
	// The SSRF guard refusing an internal address is a policy, not an outage.
	if errors.Is(err, safefetch.ErrBlocked) {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusTooManyRequests || se.code == http.StatusRequestTimeout
	}
	// SMTP: 4xx replies are transient by definition, 5xx are refusals.
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code >= 400 && te.Code < 500
	}
	// A certificate the client does not trust will not be trusted next time.
	var ce *tls.CertificateVerificationError
	if errors.As(err, &ce) {
		return false
	}
	// Anything else is the network: a refused or reset connection, a timeout.
	return true
}

// describe renders a delivery error for the panel. A transport error from
// net/http carries the request URL, and for a Discord or webhook channel the URL
// is the secret -- the API masks it everywhere else -- so it is cut out here.
func describe(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		msg := ue.Err.Error()
		// Keep the "(after N attempts)" the retry loop wrapped around it.
		if full, inner := err.Error(), ue.Error(); len(full) > len(inner) && full[:len(inner)] == inner {
			msg += full[len(inner):]
		}
		return msg
	}
	return err.Error()
}

// DeliverTo sends one event to a single channel, once. Exposed so the API can
// send a test event: that is interactive, so it gets no retries and its error
// comes back to the person who pressed the button. The error is already
// stripped of the channel's URL (see describe).
func (d *Dispatcher) DeliverTo(ctx context.Context, c *models.NotificationChannel, cfg map[string]string, e models.Event) error {
	name, slug := d.serverIdentity(e.ServerID)
	if err := d.deliverTo(ctx, c, cfg, e, name, slug); err != nil {
		return errors.New(describe(err))
	}
	return nil
}

// deliverTo is DeliverTo with the event's server already resolved.
func (d *Dispatcher) deliverTo(ctx context.Context, c *models.NotificationChannel, cfg map[string]string, e models.Event, name, slug string) error {
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
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
