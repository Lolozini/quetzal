package backup

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// A backup or a restore that ends is an event, which the notification channels
// and the server's activity log read. Only the request that started one used
// to be, so a scheduled backup that failed every night was told to no one.
func TestFinishAnnouncesTheOutcome(t *testing.T) {
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "b.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv := &models.Server{Slug: "mc", Namespace: "quetzal-srv-mc"}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("server: %v", err)
	}
	m := NewManager(st, nil)

	cases := []struct {
		dir      models.BackupDirection
		phase    models.BackupPhase
		size     int64
		msg      string
		wantType string
		wantMsg  string
	}{
		{models.DirBackup, models.BackupSucceeded, 306184192, "", models.EventBackupSucceeded, "mc: backup #%d completed (292 MiB)"},
		{models.DirBackup, models.BackupFailed, 0, "Fatal: create key in repository at the backup repository failed: Access Denied",
			models.EventBackupFailed, "mc: backup #%d failed: Fatal: create key in repository at the backup repository failed: Access Denied"},
		{models.DirRestore, models.BackupSucceeded, 0, "", models.EventRestoreSucceeded, "mc: restored backup #7"},
		{models.DirRestore, models.BackupFailed, 0, "backup job failed", models.EventRestoreFailed, "mc: restoring backup #7 failed: backup job failed"},
	}
	for _, c := range cases {
		b := &models.Backup{ServerID: srv.ID, Direction: c.dir, Phase: models.BackupRunning}
		if c.dir == models.DirRestore {
			b.SourceID = 7
		}
		if err := st.CreateBackup(b); err != nil {
			t.Fatalf("backup row: %v", err)
		}
		m.finish(b, c.phase, c.size, c.msg)

		es, err := st.ListEventsForServer(srv.ID, 0, 1)
		if err != nil || len(es) != 1 {
			t.Fatalf("%s %s: events = %v, %v", c.dir, c.phase, es, err)
		}
		want := c.wantMsg
		if c.dir == models.DirBackup {
			want = fmt.Sprintf(c.wantMsg, b.ID)
		}
		if es[0].Type != c.wantType || es[0].Message != want {
			t.Errorf("%s %s: event %s %q, want %s %q", c.dir, c.phase, es[0].Type, es[0].Message, c.wantType, want)
		}
	}
}

// A size reads the way the panel shows one.
func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{
		512:        "512 B",
		1536:       "1.5 KiB",
		306184192:  "292 MiB",
		1503238553: "1.4 GiB",
	} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
