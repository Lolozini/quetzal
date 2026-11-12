package reconciler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func orphanEvents(t *testing.T, r *Reconciler) []models.Event {
	t.Helper()
	all, err := r.Store.ListEvents(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []models.Event
	for _, e := range all {
		if e.Type == models.EventNamespaceOrphaned {
			out = append(out, e)
		}
	}
	return out
}

// A database restored from a backup orphans every server made since, and one
// line in the controller's log was all that said so in the hour before their
// volumes went. The administrators now get a report the panel shows and, once
// for each change, an event their notification channels carry.
func TestGCReportsOrphansToTheAdministrators(t *testing.T) {
	r, _, valid, now := gcFixture(t, 4, 1)
	r.Store = reconStore(t)
	r.ClusterID = 7
	ctx := context.Background()

	if err := r.GCOrphanNamespaces(ctx, valid); err != nil {
		t.Fatal(err)
	}
	reps, err := r.Store.ListOrphanReports()
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 1 || reps[0].ClusterID != 7 || len(reps[0].Names) != 1 || reps[0].Names[0] != NamespaceFor("orphan0") {
		t.Fatalf("report = %+v, want one for cluster 7 naming %s", reps, NamespaceFor("orphan0"))
	}
	if reps[0].Refused || reps[0].GraceSeconds != int64(DefaultOrphanGrace/time.Second) || reps[0].Managed != 5 {
		t.Errorf("report = %+v, want not refused, a grace of %s, 5 managed", reps[0], DefaultOrphanGrace)
	}
	evs := orphanEvents(t, r)
	if len(evs) != 1 {
		t.Fatalf("%d events after the first pass, want 1", len(evs))
	}
	if evs[0].ServerID != 0 || !strings.Contains(evs[0].Message, NamespaceFor("orphan0")) ||
		!strings.Contains(evs[0].Message, "restored") {
		t.Errorf("event = %+v, want a panel-wide one that names the namespace and says why to look", evs[0])
	}

	// The next passes find the same thing: the report stays as it was and
	// nothing is said again.
	first := reps[0].Since
	*now = now.Add(10 * time.Minute)
	if err := r.GCOrphanNamespaces(ctx, valid); err != nil {
		t.Fatal(err)
	}
	reps, _ = r.Store.ListOrphanReports()
	if len(reps) != 1 || !reps[0].Since.Equal(first) {
		t.Errorf("report changed with the same set: %+v (first seen %v)", reps, first)
	}
	if n := len(orphanEvents(t, r)); n != 1 {
		t.Errorf("%d events after a second pass over the same set, want still 1", n)
	}

	// The row comes back: nothing is left to report, and that is not an alarm.
	back := map[string]bool{"orphan0": true}
	for k := range valid {
		back[k] = true
	}
	if err := r.GCOrphanNamespaces(ctx, back); err != nil {
		t.Fatal(err)
	}
	if reps, _ = r.Store.ListOrphanReports(); len(reps) != 0 {
		t.Errorf("report kept after the server came back: %+v", reps)
	}
	if n := len(orphanEvents(t, r)); n != 1 {
		t.Errorf("%d events after the report cleared, want still 1", n)
	}
}

// Too many at once to delete on its own is the worst case of a restore, and it
// says so: the report is marked refused and the event tells to delete by hand.
func TestGCReportsAMassOrphaningAsRefused(t *testing.T) {
	r, _, valid, _ := gcFixture(t, 1, 5)
	r.Store = reconStore(t)
	r.ClusterID = 1
	if err := r.GCOrphanNamespaces(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	reps, err := r.Store.ListOrphanReports()
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 1 || !reps[0].Refused || len(reps[0].Names) != 5 {
		t.Fatalf("report = %+v, want 5 names, refused", reps)
	}
	evs := orphanEvents(t, r)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "none is deleted") {
		t.Fatalf("events = %+v, want one that says nothing is deleted", evs)
	}
}

// A second orphan is news; a pass with the same two is not.
func TestGCReportsEachChangeOfTheSetOnce(t *testing.T) {
	r, _, valid, _ := gcFixture(t, 6, 1)
	r.Store = reconStore(t)
	r.ClusterID = 1
	ctx := context.Background()
	if err := r.GCOrphanNamespaces(ctx, valid); err != nil {
		t.Fatal(err)
	}
	more := map[string]bool{}
	for k, v := range valid {
		more[k] = v
	}
	delete(more, "live0") // a second server loses its row
	for i := 0; i < 3; i++ {
		if err := r.GCOrphanNamespaces(ctx, more); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(orphanEvents(t, r)); n != 2 {
		t.Fatalf("%d events for two sets seen over four passes, want 2", n)
	}
}
