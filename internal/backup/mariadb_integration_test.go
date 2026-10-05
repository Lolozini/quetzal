package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/dbprovision"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// EnvMariaDB turns on the tests that run the database scripts against a real
// MariaDB in Docker: "1" for the release the panel deploys, or an image.
// EnvMariaDBClient runs the scripts in another image than the server's, as an
// external host is dumped with the panel's release ("1" for it).
const (
	EnvMariaDB       = "QUETZAL_TEST_MARIADB"
	EnvMariaDBClient = "QUETZAL_TEST_MARIADB_CLIENT"
)

// The dump, load and import scripts run here as their Jobs run them -- in a
// MariaDB image, as the server's own account, provisioned as the panel
// provisions it -- against a real MariaDB: the files they read and write are
// directories mounted where the Jobs mount their volumes.
func TestDatabaseScriptsAgainstMariaDB(t *testing.T) {
	image := os.Getenv(EnvMariaDB)
	if image == "" {
		t.Skipf("set %s=1 (or a MariaDB image) to run the database scripts against a real MariaDB in Docker", EnvMariaDB)
	}
	if image == "1" {
		image = reconciler.DefaultMariaDBImage
	}
	clientImage := os.Getenv(EnvMariaDBClient)
	switch clientImage {
	case "":
		clientImage = image
	case "1":
		clientImage = reconciler.DefaultMariaDBImage
	}
	t.Logf("server %s, client %s", image, clientImage)
	docker := func(stdin string, args ...string) (string, error) {
		cmd := exec.Command("docker", args...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	name := fmt.Sprintf("quetzal-test-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	if out, err := docker("", "network", "create", name); err != nil {
		t.Fatalf("docker network: %v\n%s", err, out)
	}
	t.Cleanup(func() { docker("", "network", "rm", name) })
	if out, err := docker("", "run", "-d", "--name", name, "--network", name, "-e", "MARIADB_ROOT_PASSWORD=rootpw",
		"-p", "127.0.0.1::3306", image); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { docker("", "rm", "-f", name) })
	out, err := docker("", "port", name, "3306/tcp")
	if err != nil {
		t.Fatalf("docker port: %v\n%s", err, out)
	}
	_, portText, _ := strings.Cut(strings.TrimSpace(strings.Split(out, "\n")[0]), ":")
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", out, err)
	}
	admin := dbprovision.Conn{Host: "127.0.0.1", Port: port, User: "root", Password: "rootpw"}
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Minute)
	for dbprovision.Ping(ctx, admin) != nil {
		if time.Now().After(deadline) {
			logs, _ := docker("", "logs", name)
			t.Fatalf("MariaDB did not come up:\n%s", logs)
		}
		time.Sleep(time.Second)
	}
	const dbName, user, password = "s7_aaaabbbb", "u7_aaaabbbb", "Passw0rdPassw0rdPassw0rd"
	if err := dbprovision.Provision(ctx, admin, dbName, user, "%", password); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// sql runs statements in the server's database, as root or the account.
	sql := func(asRoot bool, db, stmts string) string {
		t.Helper()
		args := []string{"exec", "-i", name, "mariadb", "-N", "-B"}
		if asRoot {
			args = append(args, "-uroot", "-prootpw")
		} else {
			args = append(args, "-u"+user, "-p"+password)
		}
		out, err := docker(stmts, append(args, db)...)
		if err != nil {
			t.Fatalf("sql: %v\n%s\n%s", err, stmts, out)
		}
		return strings.TrimSpace(out)
	}
	state := func() string {
		return sql(false, dbName, "SELECT GROUP_CONCAT(name ORDER BY id) FROM clients;"+
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE();"+
			"SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = DATABASE();"+
			"SELECT HEX(avatar) FROM clients WHERE id = 1;")
	}
	sql(false, dbName, `CREATE TABLE clients (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(64), avatar BLOB);
CREATE TABLE groups_ (id INT PRIMARY KEY, client_id INT, FOREIGN KEY (client_id) REFERENCES clients(id));
INSERT INTO clients (name, avatar) VALUES ('alice', 0x00FF10), ('bob — é', NULL);
INSERT INTO groups_ VALUES (1, 1);
CREATE VIEW v_clients AS SELECT id, name FROM clients;
CREATE TRIGGER trg BEFORE INSERT ON clients FOR EACH ROW SET NEW.name = TRIM(NEW.name);
CREATE PROCEDURE p_count() SELECT COUNT(*) FROM clients;`)
	before := state()

	dir := t.TempDir()
	mkdir := func(p string) string {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// run runs a script as its Job's container does: the image's entrypoint
	// replaced, the user the test's (so the files it writes can be removed).
	run := func(script string, env map[string]string, mounts ...string) (string, error) {
		args := []string{"run", "--rm", "--network", name, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--entrypoint", "/bin/bash"}
		for k, v := range env {
			args = append(args, "-e", k+"="+v)
		}
		for _, m := range mounts {
			args = append(args, "-v", m)
		}
		return docker("", append(args, clientImage, "-c", script)...)
	}
	env := map[string]string{"DB_HOST": name, "DB_PORT": "3306", "DB_USER": user, "DB_NAME": dbName,
		"MYSQL_PWD": password, "WIPE_QUERY": wipeQuery}

	// A backup's dump.
	dumps := mkdir(filepath.Join(dir, "backup", "dumps"))
	if out, err := run(dumpScript, env, dumps+":"+dumpsPath); err != nil {
		t.Fatalf("dump: %v\n%s", err, out)
	}
	dump, err := os.ReadFile(filepath.Join(dumps, dbName+".sql"))
	if err != nil || !strings.Contains(string(dump), "INSERT INTO `clients`") || !strings.Contains(string(dump), "PROCEDURE `p_count`") {
		t.Fatalf("the dump (%v) holds:\n%.2000s", err, dump)
	}

	// The game goes on: rows change, a table and a function appear. The
	// restore makes the database the backup's again.
	sql(false, dbName, "INSERT INTO clients (name) VALUES ('carol'); DELETE FROM groups_; CREATE TABLE later (x INT);"+
		"CREATE FUNCTION f_later() RETURNS INT DETERMINISTIC RETURN 42;")
	if state() == before {
		t.Fatal("the database did not change")
	}
	restore := mkdir(filepath.Join(dir, "restore"))
	if err := os.Rename(filepath.Join(dir, "backup", "dumps"), filepath.Join(restore, "dumps")); err != nil {
		t.Fatal(err)
	}
	if out, err := run(loadScript, env, restore+":"+restorePath+":ro"); err != nil {
		t.Fatalf("load: %v\n%s", err, out)
	}
	if got := state(); got != before {
		t.Errorf("after the restore the database is\n%s\nwant\n%s", got, before)
	}
	if got := sql(false, dbName, "SELECT COUNT(*) FROM groups_"); got != "1" {
		t.Errorf("groups_ holds %s rows after the restore, want 1", got)
	}

	// A dump made elsewhere, as root, of a database of its own name: what a
	// TeamSpeak's MariaDB gives. It loads into the server's database.
	sql(true, "", `CREATE DATABASE teamspeak; USE teamspeak;
CREATE TABLE servers (server_id INT PRIMARY KEY, server_port INT);
INSERT INTO servers VALUES (1, 9987);
CREATE VIEW v_servers AS SELECT * FROM servers;
CREATE TRIGGER t_port BEFORE INSERT ON servers FOR EACH ROW SET NEW.server_port = NEW.server_port;`)
	foreign, err := docker("", "exec", name, "mariadb-dump", "-uroot", "-prootpw", "--databases", "teamspeak")
	if err != nil || !strings.Contains(foreign, "USE `teamspeak`") || !strings.Contains(foreign, "DEFINER=`root`@`localhost`") {
		t.Fatalf("the foreign dump (%v) is not what this test is about:\n%.1500s", err, foreign)
	}
	data := mkdir(filepath.Join(dir, "data"))
	if err := os.WriteFile(filepath.Join(data, "ts3.sql"), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	importEnv := func(path, wipe string) map[string]string {
		e := map[string]string{"IMPORT_PATH": path, "IMPORT_WIPE": wipe}
		for k, v := range env {
			e[k] = v
		}
		return e
	}
	if out, err := run(importScript, importEnv("/ts3.sql", "true"), data+":/data:ro"); err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	if got := sql(false, dbName, "SELECT server_port FROM servers; SELECT COUNT(*) FROM v_servers;"+
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'clients';"+
		"SELECT DEFINER FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = DATABASE();"); got != "9987\n1\n0\n"+user+"@%" {
		t.Errorf("after the import the database holds\n%s", got)
	}

	// Gzipped, without emptying the database first.
	gz := exec.Command("sh", "-c", "printf 'CREATE TABLE extra (x INT);\\nINSERT INTO extra VALUES (7);\\n' | gzip > "+filepath.Join(data, "extra.sql.gz"))
	if out, err := gz.CombinedOutput(); err != nil {
		t.Fatalf("gzip: %v %s", err, out)
	}
	if out, err := run(importScript, importEnv("extra.sql.gz", "false"), data+":/data:ro"); err != nil {
		t.Fatalf("gzip import: %v\n%s", err, out)
	}
	if got := sql(false, dbName, "SELECT x FROM extra; SELECT COUNT(*) FROM servers;"); got != "7\n1" {
		t.Errorf("after the gzip import: %s", got)
	}

	// Failures say what went wrong, and where.
	if err := os.WriteFile(filepath.Join(data, "bad.sql"), []byte("CREATE TABLE ok1 (x INT);\n\nTHIS IS NOT SQL;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = run(importScript, importEnv("/bad.sql", "false"), data+":/data:ro")
	if msg := databaseFailure(out); err == nil || !strings.HasPrefix(msg, "the import stopped, and database "+dbName+" holds part of the file: ERROR 1064") ||
		!strings.Contains(msg, "at line 3") {
		t.Errorf("a file that is not SQL: %v, %q", err, msg)
	}
	wrong := map[string]string{}
	for k, v := range env {
		wrong[k] = v
	}
	wrong["MYSQL_PWD"] = "notthepassword"
	out, err = run(dumpScript, wrong, mkdir(filepath.Join(dir, "d2"))+":"+dumpsPath)
	if msg := databaseFailure(out); err == nil || !strings.HasPrefix(msg, "database "+dbName+" could not be dumped") || !strings.Contains(msg, "Access denied") {
		t.Errorf("a refused password: %v, %q", err, msg)
	}
	out, err = run(loadScript, env, mkdir(filepath.Join(dir, "empty"))+":"+restorePath+":ro")
	if msg := databaseFailure(out); err == nil || msg != "the backup holds no dump of database "+dbName {
		t.Errorf("a snapshot without the dump: %v, %q", err, msg)
	}
}
