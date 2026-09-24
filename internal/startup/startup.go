// Package startup tells when a game server has finished starting, the way Wings
// does: by watching its console for one of the lines its egg names
// (config.startup.done).
//
// A ready pod only says the container is up. A Minecraft server then spends a
// minute or two loading its world before it accepts a player, and reporting it
// "Running" in the meantime sends players into a refused connection and
// announces "is up and running" before it is.
package startup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Matcher is one done line. As in Wings, a line prefixed "regex:" is a regular
// expression and any other is text that must appear in an output line.
type Matcher struct {
	text []byte
	re   *regexp.Regexp
}

// Compile turns a template's done lines into matchers. An invalid expression is
// left out and reported: the others still work, and the error says why a line
// never matches instead of leaving the server waiting on it.
func Compile(lines []string) ([]Matcher, error) {
	var ms []Matcher
	var errs []error
	for _, l := range lines {
		if expr, ok := strings.CutPrefix(l, "regex:"); ok && expr != "" {
			re, err := regexp.Compile(expr)
			if err != nil {
				errs = append(errs, fmt.Errorf("startup line %q: %w", l, err))
				continue
			}
			ms = append(ms, Matcher{re: re})
			continue
		}
		if l != "" {
			ms = append(ms, Matcher{text: []byte(l)})
		}
	}
	return ms, errors.Join(errs...)
}

func (m Matcher) match(line []byte) bool {
	if m.re != nil {
		return m.re.Match(line)
	}
	return bytes.Contains(line, m.text)
}

// ansi matches the escape sequences games colour their output with.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// Match reports whether an output line is one of the done lines. The line is
// also tried without its colour codes: a game that colours "Done" would
// otherwise never match a done line written as plain text.
func Match(ms []Matcher, line []byte) bool {
	line = bytes.TrimRight(line, "\r\n")
	var plain []byte
	if bytes.IndexByte(line, 0x1b) >= 0 {
		plain = ansi.ReplaceAll(line, nil)
	}
	for _, m := range ms {
		if m.match(line) || (plain != nil && m.match(plain)) {
			return true
		}
	}
	return false
}

// maxLine is how much of one line is matched at a time. A longer line is matched
// in pieces, so a done line straddling two of them would be missed; no game
// prints its ready line past the first 64 KiB of a line.
const maxLine = 64 << 10

// Scan reads output until a done line or the end of r.
func Scan(r io.Reader, ms []Matcher) (bool, error) {
	br := bufio.NewReaderSize(r, maxLine)
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 && Match(ms, line) {
			return true, nil
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return false, nil
		default:
			return false, err
		}
	}
}

// LogOpener opens a container's output from its start, following it. The
// stream ends when the container stops or ctx is done.
type LogOpener func(ctx context.Context) (io.ReadCloser, error)

const (
	// retryAfter spaces out attempts on a log whose stream ended without a done
	// line, so a failing API call is not retried on every resync.
	retryAfter = 15 * time.Second
	// forgetAfter drops a container nobody has asked about for this long: it
	// stopped, and so did the questions.
	forgetAfter = 10 * time.Minute
)

// Watcher follows the output of starting containers, one goroutine each, until
// a done line appears, and holds the answer until it has been asked for.
type Watcher struct {
	ctx context.Context
	// Kick, if set, is called when a done line is seen, so the new phase can be
	// published without waiting for the next resync.
	Kick func()

	mu      sync.Mutex
	watches map[string]*watch // by container ID
	now     func() time.Time
}

type watch struct {
	seen    bool
	running bool
	retry   time.Time
	asked   time.Time
}

// NewWatcher returns a Watcher whose goroutines end with ctx.
func NewWatcher(ctx context.Context) *Watcher {
	return &Watcher{ctx: ctx, watches: map[string]*watch{}, now: time.Now}
}

// Seen reports whether the container has printed one of the done lines. The
// first call for a container starts following its output until deadline; later
// calls read the outcome. A true answer is given once, for the caller to keep.
func (w *Watcher) Seen(containerID string, deadline time.Time, ms []Matcher, open LogOpener) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	for id, wt := range w.watches {
		if !wt.running && now.Sub(wt.asked) > forgetAfter {
			delete(w.watches, id)
		}
	}
	wt := w.watches[containerID]
	if wt == nil {
		wt = &watch{}
		w.watches[containerID] = wt
	}
	wt.asked = now
	if wt.seen {
		delete(w.watches, containerID)
		return true
	}
	if wt.running || now.Before(wt.retry) || !now.Before(deadline) {
		return false
	}
	wt.running = true
	go w.follow(containerID, deadline, ms, open)
	return false
}

func (w *Watcher) follow(id string, deadline time.Time, ms []Matcher, open LogOpener) {
	ctx, cancel := context.WithDeadline(w.ctx, deadline)
	defer cancel()
	seen := false
	if rc, err := open(ctx); err == nil {
		// Closing the stream is what unblocks a read once ctx is done.
		stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
		seen, _ = Scan(rc, ms)
		stop()
		_ = rc.Close()
	}

	w.mu.Lock()
	wt := w.watches[id]
	if wt == nil {
		wt = &watch{asked: w.now()}
		w.watches[id] = wt
	}
	wt.running = false
	wt.seen = seen
	if !seen {
		wt.retry = w.now().Add(retryAfter)
	}
	w.mu.Unlock()

	if seen && w.Kick != nil {
		w.Kick()
	}
}
