// Package testdb picks the database a test's store runs on. By default it is
// an SQLite file in the test's temporary directory. With QUETZAL_TEST_POSTGRES
// set to the URL of a PostgreSQL server, each test gets an empty database of
// its own there instead, dropped when the test ends. The panel supports both,
// and CI runs the suites on each: until it did, only SQLite had ever been
// exercised, and the row locks that guard against races -- which SQLite
// ignores -- had never run at all.
package testdb

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for the admin connection
)

// EnvPostgres names the PostgreSQL server the tests run on, as a URL whose
// account may create databases: postgres://user:pass@host:5432/postgres.
const EnvPostgres = "QUETZAL_TEST_POSTGRES"

// Postgres reports whether the tests run on PostgreSQL.
func Postgres() bool { return os.Getenv(EnvPostgres) != "" }

// Driver is the store driver the tests run on: "postgres" or "sqlite".
func Driver() string {
	if Postgres() {
		return "postgres"
	}
	return "sqlite"
}

var seq atomic.Int64

var unsafeChars = regexp.MustCompile(`[^a-z0-9_]+`)

// DSN returns the address of a new, empty database for t: an SQLite file
// called name in its temporary directory, or a database of its own on the
// PostgreSQL server. Each call is a different database.
func DSN(t testing.TB, name string) string {
	t.Helper()
	server := os.Getenv(EnvPostgres)
	if server == "" {
		return filepath.Join(t.TempDir(), name)
	}
	u, err := url.Parse(server)
	if err != nil {
		t.Fatalf("%s: %v", EnvPostgres, err)
	}
	// Unique across the test binaries go test runs side by side, and readable
	// enough to tell whose a leftover is. PostgreSQL keeps 63 bytes of a name.
	db := fmt.Sprintf("qt_%d_%d_%s", os.Getpid(), seq.Add(1),
		unsafeChars.ReplaceAllString(strings.ToLower(t.Name()), "_"))
	if len(db) > 63 {
		db = db[:63]
	}
	admin, err := sql.Open("pgx", server)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvPostgres, err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`CREATE DATABASE "` + db + `"`); err != nil {
		t.Fatalf("create database %s: %v", db, err)
	}
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", server)
		if err != nil {
			return
		}
		defer admin.Close()
		// FORCE ends the store's own connections, which nothing closes.
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + db + `" WITH (FORCE)`)
	})
	u.Path = "/" + db
	return u.String()
}
