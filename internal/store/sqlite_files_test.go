package store

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSQLiteFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	for _, uri := range []bool{false, true} {
		name := "path"
		if uri {
			name = "URI"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "panel with space.db")
			dsn := path
			if uri {
				dsn = (&url.URL{Scheme: "file", Path: path}).String()
			}
			st, err := Open(Config{Driver: DriverSQLite, DSN: dsn, Silent: true})
			if err != nil {
				t.Fatal(err)
			}
			db, err := st.db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			if err := st.db.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
				t.Fatal(err)
			}
			if err := st.db.Exec("CREATE TABLE sensitive (value TEXT)").Error; err != nil {
				t.Fatal(err)
			}
			if err := st.db.Exec("INSERT INTO sensitive VALUES ('private')").Error; err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				info, err := os.Stat(path + suffix)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("%s permissions = %o", suffix, info.Mode().Perm())
				}
				if err := os.Chmod(path+suffix, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepareSQLiteFiles(dsn); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				info, err := os.Stat(path + suffix)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("existing %s permissions = %o", suffix, info.Mode().Perm())
				}
			}
		})
	}
}

func TestSQLiteMemoryAndReadOnlyDSNs(t *testing.T) {
	for _, dsn := range []string{":memory:", "file::memory:?cache=shared", "file:private-memory?mode=memory&cache=shared"} {
		t.Run(dsn, func(t *testing.T) {
			st, err := Open(Config{Driver: DriverSQLite, DSN: dsn, Silent: true})
			if err != nil {
				t.Fatal(err)
			}
			db, err := st.db.DB()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := st.db.Exec("CREATE TABLE memory_probe (value TEXT)").Error; err != nil {
				t.Fatal(err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "missing.db")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	if _, err := Open(Config{Driver: DriverSQLite, DSN: dsn, Silent: true}); err == nil {
		t.Fatal("read-only open created missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read-only open created file: %v", err)
	}
}
