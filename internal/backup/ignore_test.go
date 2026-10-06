package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
)

// noIgnoreFile reads a server that has no IgnoreFile.
func noIgnoreFile(context.Context, cluster.Clients, *models.Server, string) (string, error) {
	return "", nil
}

// ignoreFile reads a server whose IgnoreFile holds list.
func ignoreFile(list string) func(context.Context, cluster.Clients, *models.Server, string) (string, error) {
	return func(context.Context, cluster.Clients, *models.Server, string) (string, error) { return list, nil }
}

// A .gitignore's patterns become restic's, under the root of the server's
// files: anchored where git anchors them, at any depth where git matches at
// any depth.
func TestIgnoreListReadsAsAGitignore(t *testing.T) {
	list := strings.Join([]string{
		"# the game, which SteamCMD downloads again",
		"/Engine/",
		"FactoryGame/Content",
		"steamcmd/",
		"",
		"   ",
		"*.log",
		"!keep.log",
		"**/cache",
		"[!a]*.tmp",
		"price$1",
		"  spaced  ",
		"/",
		"\\#hash",
		"crlf\r",
	}, "\n")
	want := []string{
		"/data/Engine",
		"/data/FactoryGame/Content",
		"/data/**/steamcmd",
		"/data/**/*.log",
		"!/data/**/keep.log",
		"/data/**/cache",
		"/data/**/[^a]*.tmp",
		"/data/**/price$$1",
		"/data/**/spaced",
		"/data/**/\\#hash",
		"/data/**/crlf",
	}
	got, err := ResticExcludes(list, "/data")
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Join(want, "\n") {
		t.Errorf("excludes:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	// A restore reads the snapshot's /data as its root.
	got, err = ResticExcludes("/Engine/\n*.log\n!keep.log", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/Engine\n/**/*.log\n!/**/keep.log"; got != want {
		t.Errorf("restore excludes = %q, want %q", got, want)
	}
	if got, err := ResticExcludes("# nothing but a comment\n\n", "/data"); err != nil || got != "" {
		t.Errorf("a list of comments = %q, %v", got, err)
	}
}

// A list is refused where Wings refuses it, and where restic would.
func TestIgnoreListLimits(t *testing.T) {
	for name, list := range map[string]string{
		"larger than 32 KiB":     strings.Repeat("a", maxIgnoreBytes+1),
		"more than 256 patterns": strings.Repeat("a\n", maxIgnorePatterns+1),
		"wildcards":              strings.Repeat("*", maxIgnoreWildcards+1),
		"not a valid pattern":    "logs/[abc",
	} {
		if _, err := ResticExcludes(list, "/data"); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// At the limits, and an escaped * that is no wildcard.
	ok := strings.Repeat("a\n", maxIgnorePatterns) + "#" + strings.Repeat("c", 100)
	if _, err := ResticExcludes(ok, "/data"); err != nil {
		t.Errorf("256 patterns: %v", err)
	}
	if _, err := ResticExcludes(strings.Repeat("*", maxIgnoreWildcards)+`\*`, "/data"); err != nil {
		t.Errorf("16 wildcards and an escaped star: %v", err)
	}
}

// A backup leaves out what the server lists, and records the list.
func TestABackupLeavesOutWhatTheServerLists(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true)
	m.ReadIgnore = ignoreFile("/Engine/\n*.log\n")
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	m.Process(context.Background())
	got, _ := st.GetBackup(b.ID)
	if got.Phase != models.BackupRunning || got.Ignored != "/Engine/\n*.log\n" || got.Message != "" {
		t.Fatalf("backup = %s, ignored %q, message %q", got.Phase, got.Ignored, got.Message)
	}
	restic := jobOf(t, cs, got.JobName).Spec.Template.Spec.Containers[0]
	if e := envOf(restic)[excludesEnv]; e.Value != "/data/Engine\n/data/**/*.log" {
		t.Errorf("excludes = %q", e.Value)
	}
	if script := restic.Command[2]; !strings.Contains(script, "--exclude-file "+excludesFile) || strings.Contains(script, "/Engine") {
		t.Errorf("the patterns must reach restic as a file written from the environment, never as script:\n%s", script)
	}
	finishJob(t, cs, got.JobName, "", false)
	m.Process(context.Background())
	if got, _ = st.GetBackup(b.ID); got.Phase != models.BackupSucceeded || got.Ignored == "" {
		t.Errorf("backup = %s, ignored %q", got.Phase, got.Ignored)
	}
}

// A list that cannot be applied copies every file, and the backup says why --
// where it used to keep it, a failure in its list would leave a server without
// backups.
func TestAnUnusableIgnoreFileCopiesEverything(t *testing.T) {
	for name, read := range map[string]func(context.Context, cluster.Clients, *models.Server, string) (string, error){
		"refused": ignoreFile("logs/[abc"),
		"unreadable": func(context.Context, cluster.Clients, *models.Server, string) (string, error) {
			return "", errors.New("it is a symbolic link")
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, st, srv, _, cs := dbFixture(t, true)
			m.ReadIgnore = read
			b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
			if err := st.CreateBackup(b); err != nil {
				t.Fatal(err)
			}
			m.Process(context.Background())
			got, _ := st.GetBackup(b.ID)
			if got.Phase != models.BackupRunning || got.Ignored != "" || !strings.HasPrefix(got.Message, "every file was copied: .quetzalignore") {
				t.Fatalf("backup = %s, ignored %q, message %q", got.Phase, got.Ignored, got.Message)
			}
			restic := jobOf(t, cs, got.JobName).Spec.Template.Spec.Containers[0]
			if strings.Contains(restic.Command[2], "--exclude-file") {
				t.Errorf("an unusable list still excluded:\n%s", restic.Command[2])
			}
			// The note outlives the backup's success.
			finishJob(t, cs, got.JobName, "", false)
			m.Process(context.Background())
			if got, _ = st.GetBackup(b.ID); got.Phase != models.BackupSucceeded || !strings.HasPrefix(got.Message, "every file was copied") {
				t.Errorf("backup = %s, message %q", got.Phase, got.Message)
			}
		})
	}
}

// A backup waits a while for the server's files, as its Job waits for the
// data manager it runs beside, then copies everything.
func TestABackupWaitsForTheServersFiles(t *testing.T) {
	m, st, srv, _, _ := dbFixture(t, true)
	m.ReadIgnore = func(context.Context, cluster.Clients, *models.Server, string) (string, error) {
		return "", errNoDataManager
	}
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	m.Process(context.Background())
	if got, _ := st.GetBackup(b.ID); got.Phase != models.BackupPending {
		t.Fatalf("backup = %s, want it waiting", got.Phase)
	}
	m.Now = func() time.Time { return time.Now().Add(ignoreWait + time.Minute) }
	m.Process(context.Background())
	if got, _ := st.GetBackup(b.ID); got.Phase != models.BackupRunning || !strings.HasPrefix(got.Message, "every file was copied") {
		t.Errorf("backup = %s, message %q", got.Phase, got.Message)
	}
}

// A transfer's backup is the server's whole volume on its next cluster: it
// does not even read the list.
func TestATransfersBackupCopiesEverything(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true)
	m.ReadIgnore = func(context.Context, cluster.Clients, *models.Server, string) (string, error) {
		t.Error("a full backup read the ignore file")
		return "/Engine/", nil
	}
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending, Full: true}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	m.Process(context.Background())
	got, _ := st.GetBackup(b.ID)
	if got.Phase != models.BackupRunning || got.Ignored != "" {
		t.Fatalf("backup = %s, ignored %q", got.Phase, got.Ignored)
	}
	if strings.Contains(jobOf(t, cs, got.JobName).Spec.Template.Spec.Containers[0].Command[2], "--exclude-file") {
		t.Error("a full backup excluded paths")
	}
}

// A restore leaves alone what its backup left out: deleted to match the
// snapshot, they would have taken the game with them.
func TestARestoreLeavesAloneWhatItsBackupLeftOut(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true)
	src := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded, Ignored: "/Engine/\n*.log"}
	plain := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded}
	for _, b := range []*models.Backup{src, plain} {
		if err := st.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
	}
	restore := func(from *models.Backup) string {
		t.Helper()
		r := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending, SourceID: from.ID}
		if err := st.CreateRestore(r); err != nil {
			t.Fatal(err)
		}
		m.Process(context.Background())
		got, _ := st.GetBackup(r.ID)
		if got.Phase != models.BackupRunning {
			t.Fatalf("restore = %s: %s", got.Phase, got.Message)
		}
		restic := jobOf(t, cs, got.JobName).Spec.Template.Spec.Containers[0]
		if e := envOf(restic)[excludesEnv]; from.Ignored != "" && e.Value != "/Engine\n/**/*.log" {
			t.Errorf("excludes = %q", e.Value)
		}
		finishJob(t, cs, got.JobName, "", false)
		m.Process(context.Background())
		return restic.Command[2]
	}
	script := restore(src)
	if !strings.Contains(script, `restic restore "latest:/data"`) || !strings.Contains(script, "--target /data --delete --exclude-file "+excludesFile) {
		t.Errorf("restore of a backup that left paths out:\n%s", script)
	}
	script = restore(plain)
	if !strings.Contains(script, "--target / --delete --include /data") || strings.Contains(script, "exclude") {
		t.Errorf("restore of a full backup:\n%s", script)
	}
}

// A list that no longer reads back stops the restore before anything is
// written: restored without it, the volume would lose what it names.
func TestARestoreWithoutItsListRestoresNothing(t *testing.T) {
	m, st, srv, _, cs := dbFixture(t, true)
	src := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded, Ignored: "logs/[abc"}
	if err := st.CreateBackup(src); err != nil {
		t.Fatal(err)
	}
	r := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending, SourceID: src.ID}
	if err := st.CreateRestore(r); err != nil {
		t.Fatal(err)
	}
	m.Process(context.Background())
	got, _ := st.GetBackup(r.ID)
	if got.Phase != models.BackupFailed || !strings.HasPrefix(got.Message, "nothing was restored") {
		t.Fatalf("restore = %s: %s", got.Phase, got.Message)
	}
	if jobs, _ := cs.BatchV1().Jobs(dbNS).List(context.Background(), metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Errorf("a Job was created: %v", jobs.Items[0].Name)
	}
}
