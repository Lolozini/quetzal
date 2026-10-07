package store

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// prepareSQLiteFiles creates the database privately before SQLite writes secrets.
// SQLite gives newly created journals/WAL files the database's permissions.
// Existing sidecars must be restricted too, before the connection is opened.
func prepareSQLiteFiles(dsn string) error {
	name, query, _ := strings.Cut(dsn, "?")
	values, err := url.ParseQuery(query)
	if err != nil {
		return fmt.Errorf("SQLite options: %w", err)
	}
	if strings.HasPrefix(name, "file:") {
		u, err := url.Parse(dsn)
		if err != nil {
			return fmt.Errorf("SQLite URI: %w", err)
		}
		if u.Host != "" && u.Host != "localhost" {
			return fmt.Errorf("SQLite URI must name a local file")
		}
		name = u.Path
		if u.Opaque != "" {
			name, err = url.PathUnescape(u.Opaque)
			if err != nil {
				return fmt.Errorf("SQLite filename: %w", err)
			}
		}
	}
	if name == ":memory:" || values.Get("mode") == "memory" {
		return nil
	}
	// A read-only connection must not create a missing database or change the
	// operator's file. Read-write connections own confidentiality of their data.
	if values.Get("mode") == "ro" || values.Get("immutable") == "1" {
		return nil
	}
	flags := os.O_RDWR
	if values.Get("mode") != "rw" {
		flags |= os.O_CREATE
	}
	if err := privateSQLiteFile(name, flags); err != nil {
		return err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err := privateSQLiteFile(name+suffix, os.O_RDWR); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func privateSQLiteFile(name string, flags int) error {
	f, err := os.OpenFile(name, flags, 0o600)
	if err != nil {
		return fmt.Errorf("open private SQLite file %q: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("SQLite file %q is not a regular file", name)
	}
	if info.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			return fmt.Errorf("restrict SQLite file %q to its owner: %w", name, err)
		}
	}
	return nil
}
