package sftpactivity

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

const sidecarLog = `2026-10-03T08:00:00.000000001Z sftp: serving SFTP on :2022 (root /data)
2026-10-03T08:00:01.100000000Z sftp: "alice" write "/plugins/a.jar"
2026-10-03T08:00:01.200000000Z sftp: "alice" setstat "/plugins/a.jar"
2026-10-03T08:00:02.000000000Z sftp: bob mkdir "/world2"
2026-10-03T08:00:03.000000000Z sftp: "alice" rename "/a.txt -> /b.txt"
2026-10-03T08:00:04.000000000Z sftp: "al ice" remove "/old \"x\".log"
2026-10-03T08:00:05.000000000Z sftp: "alice" write "/cut`

func TestParseReadsTheChangesAndNothingElse(t *testing.T) {
	ops := Parse(sidecarLog, time.Time{})
	var got []string
	for _, op := range ops {
		got = append(got, op.User+"|"+op.Action+"|"+op.Path)
	}
	want := []string{
		"alice|sftp.write|/plugins/a.jar",
		"bob|sftp.mkdir|/world2",
		"alice|sftp.rename|/a.txt -> /b.txt",
		`al ice|sftp.delete|/old "x".log`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("parsed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	since := ops[1].At
	if after := Parse(sidecarLog, since); len(after) != 2 || after[0].Action != "sftp.rename" {
		t.Fatalf("after the cursor = %+v, want the rename and the remove", after)
	}
}

func TestEveryActionIsInTheCatalog(t *testing.T) {
	for _, a := range Actions {
		if !models.KnownEventType(a) {
			t.Errorf("%s is not in models.EventTypes", a)
		}
	}
	for _, a := range actions {
		found := false
		for _, b := range Actions {
			found = found || a == b
		}
		if !found {
			t.Errorf("%s is missing from Actions", a)
		}
	}
}

func TestManyChangesMakeOneEntry(t *testing.T) {
	var ops []Op
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		ops = append(ops, Op{At: at.Add(time.Duration(i) * time.Second), User: "alice", Action: "sftp.write", Path: fmt.Sprintf("/f%d", i)})
	}
	ops = append(ops, Op{At: at.Add(time.Minute), User: "bob", Action: "sftp.write", Path: "/g"})
	es := Group(ops)
	if len(es) != 2 {
		t.Fatalf("entries = %d, want one for each person", len(es))
	}
	if d := es[0].Detail(); d != "/f0, /f1, /f2, /f3, /f4 (+3 more)" {
		t.Errorf("detail = %q", d)
	}
	if !es[0].At.Equal(at.Add(7 * time.Second)) {
		t.Errorf("entry time = %v, want the last change's", es[0].At)
	}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "s.db"), Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCollectRecordsEachChangeOnce(t *testing.T) {
	st := newStore(t)
	alice := &models.User{Username: "alice", PasswordHash: "x"}
	if err := st.CreateUser(alice); err != nil {
		t.Fatal(err)
	}
	srv := &models.Server{ID: 7, Slug: "mc"}
	log := sidecarLog
	c := &Collector{
		Store: st,
		Pods:  func(context.Context, string, string) ([]string, error) { return []string{"data-0"}, nil },
		Logs: func(_ context.Context, _, _ string, since time.Time) (string, error) {
			// The API's sinceTime is to the second: hand back what came
			// before the cursor too, as the cluster does.
			return log, nil
		},
	}
	if err := c.Collect(context.Background(), srv, "quetzal-srv-mc"); err != nil {
		t.Fatal(err)
	}
	if err := c.Collect(context.Background(), srv, "quetzal-srv-mc"); err != nil {
		t.Fatal(err)
	}
	audit, err := st.ListAuditForServer(7, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 4 {
		t.Fatalf("audit entries = %d, want 4 (each change once): %+v", len(audit), audit)
	}
	events, err := st.EventsAfter(0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Type != "sftp.write" || events[0].Message != "mc: /plugins/a.jar" || events[0].UserID != alice.ID {
		t.Fatalf("events = %+v", events)
	}

	log += "\n2026-10-03T08:01:00Z sftp: \"bob\" rmdir \"/world2\"\n"
	if err := c.Collect(context.Background(), srv, "quetzal-srv-mc"); err != nil {
		t.Fatal(err)
	}
	audit, _ = st.ListAuditForServer(7, 0, 50)
	if len(audit) != 5 || audit[0].Action != "sftp.delete" || audit[0].Username != "bob" || audit[0].UserID != 0 {
		t.Fatalf("after a new line, newest = %+v (of %d)", audit[0], len(audit))
	}
}
