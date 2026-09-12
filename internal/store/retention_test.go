package store

import (
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// The event table is an outbox the dispatcher reads with a cursor and that was
// never emptied: every power action, crash and restart added a row for the life
// of the install. Pruning it must stop dead at the cursor — an event that has
// not gone out yet would become a notification nobody ever receives.
func TestDeleteEventsBeforeNeverPrunesUndelivered(t *testing.T) {
	st := newTestStore(t)
	old := time.Now().AddDate(0, 0, -60)

	var ids []uint
	for i := 0; i < 4; i++ {
		e := &models.Event{Type: "server.power", Message: "old"}
		if err := st.AddEvent(e); err != nil {
			t.Fatal(err)
		}
		// AddEvent stamps CreatedAt; age them by hand.
		if err := st.db.Model(&models.Event{}).Where("id = ?", e.ID).
			Update("created_at", old).Error; err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	recent := &models.Event{Type: "server.power", Message: "recent"}
	if err := st.AddEvent(recent); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().AddDate(0, 0, -30)

	// Nothing delivered yet: nothing may go.
	if n, err := st.DeleteEventsBefore(cutoff, 0); err != nil || n != 0 {
		t.Fatalf("with no cursor: removed %d (%v), want 0", n, err)
	}

	// Delivered through the second event: only the first two are prunable, even
	// though all four are old enough.
	n, err := st.DeleteEventsBefore(cutoff, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d, want 2 (only what is both old and delivered)", n)
	}
	left, err := st.ListEvents(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 3 {
		t.Errorf("%d events left, want 3", len(left))
	}
	// And the recent one survives regardless of the cursor.
	var sawRecent bool
	for _, e := range left {
		if e.Message == "recent" {
			sawRecent = true
		}
	}
	if !sawRecent {
		t.Error("an event newer than the cutoff was pruned")
	}
}

// Audit pruning is opt-in, but when asked for it must respect the cutoff.
func TestDeleteAuditBefore(t *testing.T) {
	st := newTestStore(t)
	mk := func(action string, age int) uint {
		e := &models.AuditEntry{Action: action, Username: "u"}
		if err := st.AddAudit(e); err != nil {
			t.Fatal(err)
		}
		if age > 0 {
			if err := st.db.Model(&models.AuditEntry{}).Where("id = ?", e.ID).
				Update("created_at", time.Now().AddDate(0, 0, -age)).Error; err != nil {
				t.Fatal(err)
			}
		}
		return e.ID
	}
	mk("server.create", 400)
	mk("server.delete", 400)
	mk("server.power", 0)

	n, err := st.DeleteAuditBefore(time.Now().AddDate(0, 0, -365))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	total, err := st.CountAudit(0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("%d entries left, want 1", total)
	}
}

// The audit log is only useful if you can reach past the newest page: a cursor
// walks the whole history, and the count says how much there is.
func TestAuditPagination(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 25; i++ {
		if err := st.AddAudit(&models.AuditEntry{Action: "a", Username: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	total, err := st.CountAudit(0)
	if err != nil || total != 25 {
		t.Fatalf("count = %d (%v), want 25", total, err)
	}

	seen := map[uint]bool{}
	var before uint
	for page := 0; page < 5; page++ {
		es, err := st.ListAudit(before, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(es) == 0 {
			break
		}
		for _, e := range es {
			if seen[e.ID] {
				t.Fatalf("entry %d returned twice — the cursor is not advancing", e.ID)
			}
			seen[e.ID] = true
		}
		before = es[len(es)-1].ID
	}
	if len(seen) != 25 {
		t.Errorf("walked %d entries, want all 25", len(seen))
	}
}
