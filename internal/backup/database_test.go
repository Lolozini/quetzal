package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

func twoDatabases() []DatabaseParams {
	return []DatabaseParams{
		{Name: "s4_aaaa", Host: "quetzal-db.quetzal-db-1.svc", Port: 3306, User: "u4_aaaa", Password: "pwA", Image: "registry.local/mariadb:11.4"},
		{Name: "s4_bbbb", Host: "10.0.0.9", Port: 3307, User: "u4_bbbb", Password: "pwB", Image: "mariadb:11.4"},
	}
}

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

// A backup of a server with databases dumps each of them before restic runs,
// with the server's own account and the client tools of the database's image,
// and restic copies the dumps with the files. The password reaches the dump
// through the operation's Secret, never the Job's spec.
func TestABackupDumpsTheServersDatabases(t *testing.T) {
	p := Params{Slug: "s4", Namespace: "quetzal-srv-s4", BackupID: 12, Direction: models.DirBackup, Image: "restic/restic:test", Databases: twoDatabases()}
	job := BuildJob(p)
	spec := job.Spec.Template.Spec

	if len(spec.InitContainers) != 2 {
		t.Fatalf("init containers = %d, want a dump per database", len(spec.InitContainers))
	}
	for i, c := range spec.InitContainers {
		db := p.Databases[i]
		env := envOf(c)
		if c.Image != db.Image || !strings.HasPrefix(c.Name, dumpContainerPrefix) {
			t.Errorf("dump %d: %s on %s, want a dump container on %s", i, c.Name, c.Image, db.Image)
		}
		if env["DB_NAME"].Value != db.Name || env["DB_HOST"].Value != db.Host || env["DB_USER"].Value != db.User || env["DB_PORT"].Value == "" {
			t.Errorf("dump %d env = %+v", i, env)
		}
		pw := env["MYSQL_PWD"]
		if pw.Value != "" || pw.ValueFrom == nil || pw.ValueFrom.SecretKeyRef == nil || pw.ValueFrom.SecretKeyRef.Key != dbPasswordKey(i) {
			t.Errorf("dump %d: the password is %+v, want it from the Secret", i, pw)
		}
		if strings.Contains(c.Command[2], db.Password) {
			t.Errorf("dump %d: the password is in the script", i)
		}
		if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != dumpsPath || c.VolumeMounts[0].Name == "data" {
			t.Errorf("dump %d mounts %+v, want the dumps volume alone", i, c.VolumeMounts)
		}
	}
	restic := spec.Containers[0]
	if !strings.Contains(restic.Command[2], "restic backup /data /dumps ") {
		t.Errorf("restic does not copy the dumps:\n%s", restic.Command[2])
	}
	mounted := map[string]bool{}
	for _, m := range restic.VolumeMounts {
		mounted[m.MountPath] = m.ReadOnly
	}
	if ro, ok := mounted[dumpsPath]; !ok || !ro {
		t.Errorf("restic mounts %+v, want the dumps read-only", restic.VolumeMounts)
	}
	sec := BuildSecret(p)
	if sec.StringData[dbPasswordKey(0)] != "pwA" || sec.StringData[dbPasswordKey(1)] != "pwB" || sec.StringData["RESTIC_PASSWORD"] != p.RepoPassword {
		t.Errorf("secret = %v", sec.StringData)
	}

	// Without databases, the Job is what it was.
	plain := BuildJob(Params{Slug: "s4", BackupID: 13, Direction: models.DirBackup})
	if n := len(plain.Spec.Template.Spec.InitContainers); n != 0 {
		t.Errorf("a server without databases got %d init containers", n)
	}
	if !strings.Contains(plain.Spec.Template.Spec.Containers[0].Command[2], "restic backup /data --host") {
		t.Errorf("without databases the backup copies more than the volume:\n%s", plain.Spec.Template.Spec.Containers[0].Command[2])
	}
}

// A restore asked for the databases runs restic first -- the files, and the
// dumps beside them -- then loads each database back, in order: init
// containers stop at the first that fails.
func TestARestoreLoadsTheDatabasesBackAfterTheFiles(t *testing.T) {
	p := Params{Slug: "s4", BackupID: 20, SourceID: 12, Direction: models.DirRestore, Image: "restic/restic:test", Databases: twoDatabases(), InstallGen: 2}
	spec := BuildJob(p).Spec.Template.Spec

	if len(spec.InitContainers) != 3 || spec.InitContainers[0].Name != "restic" {
		t.Fatalf("init containers = %v, want restic then a load per database", containerNames(spec.InitContainers))
	}
	script := spec.InitContainers[0].Command[2]
	for _, want := range []string{`--target / --delete --include /data`, `--target /restore --include /dumps`, ".quetzal-installed"} {
		if !strings.Contains(script, want) {
			t.Errorf("restic's restore misses %q:\n%s", want, script)
		}
	}
	for i, c := range spec.InitContainers[1:] {
		if c.Image != p.Databases[i].Image || envOf(c)["DB_NAME"].Value != p.Databases[i].Name || !strings.HasPrefix(c.Name, loadContainerPrefix) {
			t.Errorf("load %d = %s on %s", i, c.Name, c.Image)
		}
		if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != restorePath || !c.VolumeMounts[0].ReadOnly {
			t.Errorf("load %d mounts %+v", i, c.VolumeMounts)
		}
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Name != "done" {
		t.Errorf("containers = %v", containerNames(spec.Containers))
	}
	if spec.Affinity != nil {
		t.Error("a restore is not pinned beside the data manager: it waits for the volume to be free")
	}
	for _, m := range spec.InitContainers[0].VolumeMounts {
		if m.Name == "data" && m.ReadOnly {
			t.Error("restic restores into a read-only volume")
		}
	}

	// Files only: no load, and no dumps extracted.
	files := BuildJob(Params{Slug: "s4", BackupID: 21, SourceID: 12, Direction: models.DirRestore})
	if s := files.Spec.Template.Spec; len(s.InitContainers) != 0 || strings.Contains(s.Containers[0].Command[2], restorePath) {
		t.Errorf("a restore of the files alone touches the dumps: %v\n%s", containerNames(s.InitContainers), s.Containers[0].Command[2])
	}
}

// An import reads its file from the server's volume, read-only and beside the
// data manager that holds it, and is given the database's password and
// nothing of the backup target.
func TestAnImportReadsTheVolumeAndKnowsOnlyItsDatabase(t *testing.T) {
	p := Params{
		Slug: "s4", Namespace: "quetzal-srv-s4", BackupID: 30, Direction: models.DirDatabaseImport,
		Databases: twoDatabases()[:1], ImportPath: "/dumps/ts3.sql.gz", ImportWipe: true,
		Repository: "s3:https://s3.example/bucket/s4", RepoPassword: "repo", AccessKey: "ak", SecretKey: "sk",
	}
	job := BuildJob(p)
	if job.Name != "quetzal-db-import-30" {
		t.Errorf("job name = %s", job.Name)
	}
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 0 || len(spec.Containers) != 1 || spec.Containers[0].Name != importContainerName {
		t.Fatalf("containers = %v / %v", containerNames(spec.InitContainers), containerNames(spec.Containers))
	}
	c := spec.Containers[0]
	env := envOf(c)
	if env["IMPORT_PATH"].Value != "/dumps/ts3.sql.gz" || env["IMPORT_WIPE"].Value != "true" || c.Image != "registry.local/mariadb:11.4" {
		t.Errorf("import env = %+v on %s", env, c.Image)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/data" || !c.VolumeMounts[0].ReadOnly {
		t.Errorf("import mounts %+v, want the volume read-only", c.VolumeMounts)
	}
	if v := spec.Volumes[0]; v.PersistentVolumeClaim == nil || !v.PersistentVolumeClaim.ReadOnly {
		t.Errorf("volume = %+v, want the claim read-only", v)
	}
	if spec.Affinity == nil || spec.Affinity.PodAffinity == nil ||
		spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchLabels[reconciler.DataLabel] != "s4" {
		t.Errorf("an import must land beside the data manager: %+v", spec.Affinity)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the import's pod mounts a service account token")
	}
	sec := BuildSecret(p)
	if len(sec.StringData) != 1 || sec.StringData[dbPasswordKey(0)] != "pwA" {
		t.Errorf("an import's secret = %v, want the database's password alone", sec.StringData)
	}
	// It runs once: a second attempt would load the file twice, and report
	// its own error in place of the first's.
	if b := job.Spec.BackoffLimit; b == nil || *b != 0 {
		t.Errorf("an import's backoff limit = %v, want 0", b)
	}
	if b := BuildJob(Params{Slug: "s4", BackupID: 31, Direction: models.DirBackup}).Spec.BackoffLimit; b == nil || *b != 1 {
		t.Errorf("a backup's backoff limit = %v, want its retry kept", b)
	}
}

func containerNames(cs []corev1.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// A failed database step is told by its own Fatal line, which names the
// database and carries the client's error, not by the object store's causes.
func TestDatabaseFailureIsTheStepsOwnLine(t *testing.T) {
	logs := "WARNING: option --ssl-verify-server-cert is disabled, because of an insecure passwordless login.\n" +
		"mariadb-dump: Got error: 2002: \"Can't connect to server on 'db' (115)\" when trying to connect\n" +
		"Fatal: database s4_aaaa could not be dumped: mariadb-dump: Got error: 2002: \"Can't connect to server on 'db' (115)\" when trying to connect\n"
	if got := databaseFailure(logs); got != `database s4_aaaa could not be dumped: mariadb-dump: Got error: 2002: "Can't connect to server on 'db' (115)" when trying to connect` {
		t.Errorf("databaseFailure = %q", got)
	}
	if got := databaseFailure("WARNING: something\nbash: mariadb-dump: command not found\n"); got != "bash: mariadb-dump: command not found" {
		t.Errorf("without a Fatal line = %q", got)
	}
	for _, name := range []string{"dump-0", "load-3", "import"} {
		if !isDatabaseContainer(name) {
			t.Errorf("%s is not taken for a database step", name)
		}
	}
	for _, name := range []string{"restic", "done", ""} {
		if isDatabaseContainer(name) {
			t.Errorf("%s is taken for a database step", name)
		}
	}
}

// The import resolves its file inside the volume and refuses one that is not
// there, leads out of it, is not a file, or is a damaged gzip -- before it
// touches the database. A file it accepts reaches the client with the lines
// that switch database blanked (not removed: an error's line number is the
// file's) and the definers left out. The volume is a directory here, and the
// client a script that records what it is given.
func TestTheImportScriptFindsItsFileAndMakesAForeignDumpFit(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	bin := filepath.Join(root, "bin")
	for _, d := range []string{data, bin, filepath.Join(data, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dump := "-- dump\nCREATE DATABASE /*!32312 IF NOT EXISTS*/ `teamspeak` /*!40100 DEFAULT CHARACTER SET utf8mb4 */;\n\nUSE `teamspeak`;\n" +
		"CREATE TABLE servers (id INT);\n/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\nINSERT INTO servers VALUES (1);\n"
	files := map[string]string{"ts3.sql": dump, "sub/ok.sql": "SELECT 1;\n", "broken.sql.gz": "not gzip at all"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(data, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gz := exec.Command("sh", "-c", "printf 'SELECT 2;\\n' | gzip > "+filepath.Join(data, "ok.sql.gz"))
	if out, err := gz.CombinedOutput(); err != nil {
		t.Fatalf("gzip: %v %s", err, out)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(data, "escape.sql")); err != nil {
		t.Fatal(err)
	}
	// The client: answers the connection check, lists one table to drop when
	// asked what the database holds, and keeps what it is sent otherwise.
	client := `#!/bin/bash
for a in "$@"; do case "$a" in -e) e=1 ;; esac; done
if [ -n "${e:-}" ]; then
  case "$*" in *information_schema*) echo 'DROP TABLE IF EXISTS ` + "`old`" + `;' ;; esac
  exit 0
fi
cat >> "$LOADED"
`
	if err := os.WriteFile(filepath.Join(bin, "mariadb"), []byte(client), 0o755); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(importScript, `"/data/`, `"`+data+`/`)
	script = strings.ReplaceAll(script, `/data/*)`, data+`/*)`)

	run := func(path, wipe string) (string, string) {
		t.Helper()
		loaded := filepath.Join(root, "loaded")
		os.Remove(loaded)
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LOADED=" + loaded, "IMPORT_PATH=" + path, "IMPORT_WIPE=" + wipe,
			"DB_HOST=db", "DB_PORT=3306", "DB_USER=u4_aaaa", "DB_NAME=s4_aaaa", "MYSQL_PWD=x", "WIPE_QUERY=" + wipeQuery}
		out, _ := cmd.CombinedOutput()
		got, _ := os.ReadFile(loaded)
		return databaseFailure(string(out)), string(got)
	}

	for path, want := range map[string]string{
		"/missing.sql":    "/missing.sql is not in the server's files",
		"escape.sql":      "escape.sql leads outside the server's files",
		"../../etc/hosts": "../../etc/hosts is not in the server's files",
		"/sub":            "/sub is not a file",
		"/broken.sql.gz":  "/broken.sql.gz is not a gzip file, or it is damaged",
	} {
		if strings.HasPrefix(path, "../") {
			// Leads out of the volume through the path itself.
			if _, err := os.Stat(filepath.Join(data, path)); err == nil {
				want = path + " leads outside the server's files"
			}
		}
		msg, loaded := run(path, "false")
		if msg != want || loaded != "" {
			t.Errorf("import of %s: %q, loaded %q; want %q and nothing loaded", path, msg, loaded, want)
		}
	}

	msg, loaded := run("/ts3.sql", "true")
	if msg != "" {
		t.Fatalf("import of ts3.sql failed: %s", msg)
	}
	wantLoaded := "SET FOREIGN_KEY_CHECKS=0;\nDROP TABLE IF EXISTS `old`;\n" +
		"-- dump\n\n\n\nCREATE TABLE servers (id INT);\n/*!50013  SQL SECURITY DEFINER */\nINSERT INTO servers VALUES (1);\n"
	if loaded != wantLoaded {
		t.Errorf("the client was sent:\n%s\nwant:\n%s", loaded, wantLoaded)
	}
	if strings.Count(loaded, "\n")-2 != strings.Count(dump, "\n") {
		t.Error("the import changed the number of lines: an error's line number would not be the file's")
	}
	if msg, loaded := run("ok.sql.gz", "false"); msg != "" || loaded != "SELECT 2;\n" {
		t.Errorf("a gzip file: %q, loaded %q", msg, loaded)
	}
}
