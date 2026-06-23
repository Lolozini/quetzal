// Package dbprovision creates and removes databases and scoped users on a
// MySQL/MariaDB server, for the per-server "databases" feature. MySQL can't
// parameterize identifiers, so every name is validated against a strict
// allow-list and the credentials it generates use a safe character set — there
// is no path for caller- or user-supplied text to reach a statement unescaped.
package dbprovision

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Conn describes an admin connection to a database server.
type Conn struct {
	Host     string
	Port     int
	User     string
	Password string
}

var (
	// ErrInvalidName rejects a database or user name that isn't a plain
	// identifier (defense in depth: generated names always pass).
	ErrInvalidName = errors.New("dbprovision: invalid identifier")
	// ErrInvalidRemote rejects an unexpected allowed-from host pattern.
	ErrInvalidRemote = errors.New("dbprovision: invalid remote host pattern")
	// ErrInvalidPassword rejects a password outside the safe set.
	ErrInvalidPassword = errors.New("dbprovision: invalid password")
)

const identAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// validIdent allows only [A-Za-z0-9_], 1..64 chars: safe to backtick-quote.
func validIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// validRemote allows the MySQL user "host" part: letters, digits and % . _ - :
// (covers "%", "10.0.%", "host.internal"). No quotes can appear.
func validRemote(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '%' || c == '.' || c == '_' || c == '-' || c == ':') {
			return false
		}
	}
	return true
}

// validPassword allows only [A-Za-z0-9] (8..128): safe inside '...'. Generated
// passwords use this set, so no escaping is ever required.
func validPassword(s string) bool {
	if len(s) < 8 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func randString(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(identAlphabet)))
	for i := range b {
		k, _ := rand.Int(rand.Reader, max)
		b[i] = identAlphabet[k.Int64()]
	}
	return string(b)
}

// GenerateName returns "<prefix><serverID>_<rand>" trimmed to a valid identifier
// (e.g. "s12_a8f3kd9q"). prefix should be a single safe letter.
func GenerateName(prefix string, serverID uint) string {
	return fmt.Sprintf("%s%d_%s", prefix, serverID, randString(8))
}

// GeneratePassword returns a 24-char alphanumeric password.
func GeneratePassword() string { return randString(24) }

func (c Conn) config(dbName string) *mysql.Config {
	cfg := mysql.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	cfg.DBName = dbName
	cfg.AllowNativePasswords = true
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	return cfg
}

func (c Conn) open() (*sql.DB, error) {
	db, err := sql.Open("mysql", c.config("").FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(30 * time.Second)
	return db, nil
}

// Ping verifies the admin connection works (used by the host "test" action).
func Ping(ctx context.Context, c Conn) error {
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	return db.PingContext(ctx)
}

// Provision creates the database, the scoped user and grants. It is idempotent
// (IF NOT EXISTS), so a retried create doesn't fail.
func Provision(ctx context.Context, c Conn, dbName, user, remote, password string) error {
	if !validIdent(dbName) || !validIdent(user) {
		return ErrInvalidName
	}
	if !validRemote(remote) {
		return ErrInvalidRemote
	}
	if !validPassword(password) {
		return ErrInvalidPassword
	}
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	stmts := []string{
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", dbName),
		fmt.Sprintf("CREATE USER IF NOT EXISTS `%s`@'%s' IDENTIFIED BY '%s'", user, remote, password),
		fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO `%s`@'%s'", dbName, user, remote),
		"FLUSH PRIVILEGES",
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// Deprovision drops the user and the database (best-effort: it tries both even
// if one fails, returning the first error).
func Deprovision(ctx context.Context, c Conn, dbName, user, remote string) error {
	if !validIdent(dbName) || !validIdent(user) {
		return ErrInvalidName
	}
	if !validRemote(remote) {
		return ErrInvalidRemote
	}
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	var firstErr error
	for _, q := range []string{
		fmt.Sprintf("DROP USER IF EXISTS `%s`@'%s'", user, remote),
		fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName),
		"FLUSH PRIVILEGES",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RotatePassword sets a new password for an existing user.
func RotatePassword(ctx context.Context, c Conn, user, remote, password string) error {
	if !validIdent(user) {
		return ErrInvalidName
	}
	if !validRemote(remote) {
		return ErrInvalidRemote
	}
	if !validPassword(password) {
		return ErrInvalidPassword
	}
	db, err := c.open()
	if err != nil {
		return err
	}
	defer db.Close()
	q := fmt.Sprintf("ALTER USER `%s`@'%s' IDENTIFIED BY '%s'", user, remote, password)
	if _, err := db.ExecContext(ctx, q); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "FLUSH PRIVILEGES")
	return err
}
