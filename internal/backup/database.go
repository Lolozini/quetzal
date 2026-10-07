package backup

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/lolozini/quetzal/internal/models"
)

// A backup copied the server's volume and nothing else: a server whose state
// lives in a database -- TeamSpeak, a Minecraft server's plugins -- lost all of
// it with the database host, backups or not. A backup now dumps each of the
// server's databases into its snapshot, next to the files, and a restore can
// load them back. An import loads an SQL file from the server's files into one
// of its databases, which took kubectl and a client of one's own.
//
// Every step connects with the server's own account, which holds every
// privilege on its database and none elsewhere, and uses the MariaDB client
// tools of a MariaDB image.

// DatabaseParams is one of a server's databases, as a Job reaches it.
type DatabaseParams struct {
	// Name is the database, and its dump's file name.
	Name string
	Host string
	Port int
	User string
	// Password goes to the operation's Secret, never into the Job's spec.
	Password string
	// Image holds the client tools (mariadb, mariadb-dump): the database
	// host's own image for a managed host, a MariaDB release otherwise.
	Image string
}

const (
	dumpsVolume = "dumps"
	// dumpsPath is where a backup writes its dumps, and where they sit in the
	// snapshot: /dumps/<database>.sql.
	dumpsPath = "/dumps"
	// restorePath is where a restore extracts them: /restore/dumps/<database>.sql.
	restorePath = "/restore"
)

// The containers that work on a database are named for it, so a failure there
// is told as the database's, not the object store's.
const (
	dumpContainerPrefix = "dump-"
	loadContainerPrefix = "load-"
	importContainerName = "import"
)

func dbPasswordKey(i int) string { return fmt.Sprintf("QUETZAL_DB_%d_PASSWORD", i) }

// dbEnv is how a database container is told what to reach. The password comes
// from the operation's Secret; the rest is not secret (the server's users see
// it in the panel).
func dbEnv(p Params, i int, db DatabaseParams) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "DB_HOST", Value: db.Host},
		{Name: "DB_PORT", Value: strconv.Itoa(db.Port)},
		{Name: "DB_USER", Value: db.User},
		{Name: "DB_NAME", Value: db.Name},
		{Name: "MYSQL_PWD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(p)},
			Key:                  dbPasswordKey(i),
		}}},
		{Name: "WIPE_QUERY", Value: wipeQuery},
	}
}

// wipeQuery lists the statements that empty a database: its tables and views,
// its routines and its events. A restore makes the database the backup's, as
// it makes the files the backup's: what was created since goes.
var wipeQuery = strings.ReplaceAll(`SELECT CONCAT('DROP ', IF(TABLE_TYPE = 'VIEW', 'VIEW', 'TABLE'), ' IF EXISTS §', REPLACE(TABLE_NAME, '§', '§§'), '§;') FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE()
UNION ALL
SELECT CONCAT('DROP ', ROUTINE_TYPE, ' IF EXISTS §', REPLACE(ROUTINE_NAME, '§', '§§'), '§;') FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = DATABASE()
UNION ALL
SELECT CONCAT('DROP EVENT IF EXISTS §', REPLACE(EVENT_NAME, '§', '§§'), '§;') FROM information_schema.EVENTS WHERE EVENT_SCHEMA = DATABASE()`, "§", "`")

// dbPrelude starts every database script. The client asks for TLS and takes it
// when the server offers it, without checking the server's certificate: the
// MariaDB 11.4 client otherwise insists on a verified one, and refused both a
// MariaDB 10.6 without TLS and a MySQL 8.4 with its self-signed certificate.
// The panel's own provisioning connection does not ask for TLS at all.
//
// Input SQL is untrusted: disable client-side commands and LOCAL INFILE reads.
// These are client options, not dump options, so they stay out of conn.
//
// fatal ends the step with a "Fatal:" line, which is what the operation's
// message is read from; lasterr picks the client's error out of its output.
const dbPrelude = `set -euo pipefail
fatal() { echo "Fatal: $*" >&2; exit 1; }
err="$(mktemp)"
lasterr() { grep -v '^WARNING' "$err" | tail -n 1 || true; }
conn=(--host="$DB_HOST" --port="$DB_PORT" --user="$DB_USER" --disable-ssl-verify-server-cert)
connect() {
  mariadb --binary-mode --local-infile=0 "${conn[@]}" -e 'SELECT 1' "$DB_NAME" >/dev/null 2>"$err" ||
    fatal "could not connect to database $DB_NAME: $(lasterr)"
}
wipe() {
  local drops
  drops="$(mariadb --binary-mode --local-infile=0 "${conn[@]}" -N -B -r -e "$WIPE_QUERY" "$DB_NAME" 2>"$err")" ||
    fatal "could not list what database $DB_NAME holds: $(lasterr)"
  if [ -n "$drops" ]; then
    printf 'SET FOREIGN_KEY_CHECKS=0;\n%s\n' "$drops" | mariadb --binary-mode --local-infile=0 "${conn[@]}" "$DB_NAME" 2>"$err" ||
      fatal "database $DB_NAME could not be emptied: $(lasterr)"
  fi
}
`

// dumpScript dumps one database into the snapshot's dumps. --single-transaction
// copies a consistent state of a database the game is still writing to, without
// locking it; routines, triggers and events go with the tables.
const dumpScript = dbPrelude + `
out="` + dumpsPath + `/$DB_NAME.sql"
mariadb-dump "${conn[@]}" --single-transaction --quick --routines --triggers --events \
  --hex-blob --no-tablespaces --default-character-set=utf8mb4 --result-file="$out" "$DB_NAME" 2>"$err" ||
  { cat "$err" >&2; fatal "database $DB_NAME could not be dumped: $(lasterr)"; }
`

// loadScript loads one database back from a restored dump: emptied first, then
// the dump. A load that fails half way leaves the database half loaded, and
// says so.
const loadScript = dbPrelude + `
f="` + restorePath + dumpsPath + `/$DB_NAME.sql"
[ -f "$f" ] || fatal "the backup holds no dump of database $DB_NAME"
connect
wipe
mariadb --binary-mode --local-infile=0 "${conn[@]}" "$DB_NAME" <"$f" 2>"$err" ||
  fatal "database $DB_NAME could not be loaded, and holds part of the backup: $(lasterr)"
`

// importScript loads an SQL file of the server's into one of its databases.
// The file is resolved inside the volume first, and refused if it leads out of
// it: the path is the user's, and a symlink in the volume is the game's.
// A .gz file is read through gzip.
//
// A dump made elsewhere is made to fit: one taken with --databases switches to
// its own database (CREATE DATABASE, USE), and one taken as root names root as
// the definer of its views and triggers. The server's account may do neither,
// so those lines are blanked -- not removed, so that a line number in an error
// is the file's -- and the definers left out, which makes them the account.
var importScript = dbPrelude + strings.ReplaceAll(`
src="/data/${IMPORT_PATH#/}"
real="$(realpath -e -- "$src" 2>/dev/null)" || fatal "$IMPORT_PATH is not in the server's files"
case "$real" in /data/*) ;; *) fatal "$IMPORT_PATH leads outside the server's files" ;; esac
[ -f "$real" ] || fatal "$IMPORT_PATH is not a file"
[ -r "$real" ] || fatal "$IMPORT_PATH cannot be read"
case "$real" in
  *.gz)
    gzip -t -- "$real" 2>/dev/null || fatal "$IMPORT_PATH is not a gzip file, or it is damaged"
    sql() { gzip -dc -- "$real"; } ;;
  *) sql() { cat -- "$real"; } ;;
esac
connect
if [ "$IMPORT_WIPE" = true ]; then wipe; fi
sql | sed -E \
  -e 's/^CREATE DATABASE .*$//' \
  -e 's/^USE §([^§]|§§)*§;[[:space:]]*$//' \
  -e 's/DEFINER=§([^§]|§§)*§@§([^§]|§§)*§//g' |
  mariadb --binary-mode --local-infile=0 "${conn[@]}" "$DB_NAME" 2>"$err" ||
  fatal "the import stopped, and database $DB_NAME holds part of the file: $(lasterr)"
`, "§", "`")

func bashContainer(name, image, script string, env []corev1.EnvVar, mounts []corev1.VolumeMount) corev1.Container {
	return corev1.Container{
		Name:         name,
		Image:        image,
		Command:      []string{"/bin/bash", "-c", script},
		Env:          env,
		VolumeMounts: mounts,
	}
}

// dumpContainers dump each database into the dumps volume, before restic
// copies it with the files.
func dumpContainers(p Params) []corev1.Container {
	out := make([]corev1.Container, 0, len(p.Databases))
	for i, db := range p.Databases {
		out = append(out, bashContainer(fmt.Sprintf("%s%d", dumpContainerPrefix, i), db.Image, dumpScript,
			dbEnv(p, i, db), []corev1.VolumeMount{{Name: dumpsVolume, MountPath: dumpsPath}}))
	}
	return out
}

// loadContainers load each database back from the dumps restic extracted.
func loadContainers(p Params) []corev1.Container {
	out := make([]corev1.Container, 0, len(p.Databases))
	for i, db := range p.Databases {
		out = append(out, bashContainer(fmt.Sprintf("%s%d", loadContainerPrefix, i), db.Image, loadScript,
			dbEnv(p, i, db), []corev1.VolumeMount{{Name: dumpsVolume, MountPath: restorePath, ReadOnly: true}}))
	}
	return out
}

func dumpsVolumeSource() corev1.Volume {
	return corev1.Volume{Name: dumpsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

// importContainer loads an SQL file into a database. It reads the server's
// volume read-only, beside the data manager that holds it, as a backup does.
func importContainer(p Params, mounts []corev1.VolumeMount) corev1.Container {
	db := p.Databases[0]
	wipe := "false"
	if p.ImportWipe {
		wipe = "true"
	}
	env := append(dbEnv(p, 0, db),
		corev1.EnvVar{Name: "IMPORT_PATH", Value: p.ImportPath},
		corev1.EnvVar{Name: "IMPORT_WIPE", Value: wipe},
	)
	return bashContainer(importContainerName, db.Image, importScript, env, mounts)
}

// isDatabaseContainer reports whether a container worked on a database.
func isDatabaseContainer(name string) bool {
	return strings.HasPrefix(name, dumpContainerPrefix) || strings.HasPrefix(name, loadContainerPrefix) ||
		name == importContainerName
}

// databaseFailure says why a database step failed: its "Fatal:" line, which
// names the database and carries the client's error. None of the object
// store's causes apply to it.
func databaseFailure(logs string) string {
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "Fatal:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "Fatal:"))
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "WARNING") {
			return l
		}
	}
	return ""
}

// DatabaseNames lists the databases of an operation, for its record.
func DatabaseNames(dbs []DatabaseParams) []string {
	out := make([]string, 0, len(dbs))
	for _, d := range dbs {
		out = append(out, d.Name)
	}
	return out
}

// importsDirection reports whether an operation is a database import.
func importsDirection(p Params) bool { return p.Direction == models.DirDatabaseImport }
