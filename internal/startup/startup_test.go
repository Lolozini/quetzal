package startup

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMatchLikeWings(t *testing.T) {
	ms, err := Compile([]string{")! For help, type ", "regex:^\\[Server\\] Listening on port \\d+$", ""})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("want 2 matchers (the empty line is none), got %d", len(ms))
	}
	cases := []struct {
		line string
		want bool
	}{
		// Text is matched anywhere in the line, and taken literally: the
		// parenthesis is not the start of a group.
		{`[12:00:01 INFO]: Done (3.2s)! For help, type "help"` + "\n", true},
		{"[Server] Listening on port 7777\r\n", true},
		{"[Server] Listening on port many\n", false},
		{"Loading world\n", false},
		// Coloured output still matches a done line written as plain text.
		{"\x1b[32m[12:00:01 INFO]: Done (3.2s)! For help, type \"help\"\x1b[0m\n", true},
	}
	for _, c := range cases {
		if got := Match(ms, []byte(c.line)); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestCompileKeepsTheValidLines(t *testing.T) {
	ms, err := Compile([]string{"regex:([", "Server started"})
	if err == nil || !strings.Contains(err.Error(), "regex:([") {
		t.Errorf("an invalid expression must be reported with the line, got %v", err)
	}
	if len(ms) != 1 || !Match(ms, []byte("Server started")) {
		t.Error("the valid line must still be matched")
	}
}

func TestScan(t *testing.T) {
	ms, _ := Compile([]string{"Done ("})
	long := strings.Repeat("x", 3*maxLine)
	cases := []struct {
		name, out string
		want      bool
	}{
		{"found", "Starting\nPreparing level\nDone (4.1s)!\nPlayer joined\n", true},
		{"not yet", "Starting\nPreparing level\n", false},
		{"after a very long line", "Starting\n" + long + "\nDone (4.1s)!\n", true},
		{"last line without a newline", "Starting\nDone (4.1s)!", true},
	}
	for _, c := range cases {
		got, err := Scan(strings.NewReader(c.out), ms)
		if err != nil || got != c.want {
			t.Errorf("%s: Scan = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

// pipeOpener hands out a stream the test writes the game's output into.
func pipeOpener(opened *atomic.Int32) (LogOpener, *io.PipeWriter) {
	pr, pw := io.Pipe()
	return func(ctx context.Context) (io.ReadCloser, error) {
		opened.Add(1)
		return pr, nil
	}, pw
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestWatcherFollowsUntilTheDoneLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWatcher(ctx)
	var kicks atomic.Int32
	w.Kick = func() { kicks.Add(1) }
	ms, _ := Compile([]string{"Done ("})
	var opened atomic.Int32
	open, pw := pipeOpener(&opened)
	deadline := time.Now().Add(time.Hour)

	if w.Seen("c1", deadline, ms, open) {
		t.Fatal("nothing was printed yet")
	}
	_, _ = io.WriteString(pw, "Preparing level\n")
	// Asking again does not open a second stream.
	if w.Seen("c1", deadline, ms, open) {
		t.Fatal("the done line has not been printed")
	}
	_, _ = io.WriteString(pw, "Done (3.0s)!\n")
	eventually(t, func() bool { return kicks.Load() == 1 })

	if !w.Seen("c1", deadline, ms, open) {
		t.Fatal("the done line was printed")
	}
	if opened.Load() != 1 {
		t.Errorf("the log was opened %d times, want once", opened.Load())
	}
	// The answer is handed over once; the caller keeps it.
	w.mu.Lock()
	_, kept := w.watches["c1"]
	w.mu.Unlock()
	if kept {
		t.Error("a delivered answer must be forgotten")
	}
}

func TestWatcherRetriesAStreamThatEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWatcher(ctx)
	clock := time.Now()
	var nowNanos atomic.Int64
	nowNanos.Store(clock.UnixNano())
	w.now = func() time.Time { return time.Unix(0, nowNanos.Load()) }
	ms, _ := Compile([]string{"Done ("})
	var opened atomic.Int32
	failing := func(ctx context.Context) (io.ReadCloser, error) {
		opened.Add(1)
		return nil, errors.New("the API is unavailable")
	}
	deadline := clock.Add(time.Hour)

	w.Seen("c1", deadline, ms, failing)
	eventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return !w.watches["c1"].running
	})
	w.Seen("c1", deadline, ms, failing)
	if opened.Load() != 1 {
		t.Fatalf("retried before the delay: opened %d times", opened.Load())
	}
	nowNanos.Store(clock.Add(retryAfter + time.Second).UnixNano())
	w.Seen("c1", deadline, ms, failing)
	eventually(t, func() bool { return opened.Load() == 2 })
}

func TestWatcherStopsAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWatcher(ctx)
	ms, _ := Compile([]string{"Done ("})
	var opened atomic.Int32
	open, pw := pipeOpener(&opened)
	defer pw.Close()

	// Past the deadline, nothing is followed any more.
	if w.Seen("late", time.Now().Add(-time.Second), ms, open) || opened.Load() != 0 {
		t.Fatal("a container past its deadline must not be followed")
	}
	// A stream still open at the deadline is closed then.
	w.Seen("c1", time.Now().Add(50*time.Millisecond), ms, open)
	eventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		wt := w.watches["c1"]
		return wt != nil && !wt.running
	})
}
