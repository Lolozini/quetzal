package store

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// The environment the database settings come from, the same for the
// apiserver, the controller and qctl.
const (
	EnvDriver      = "QUETZAL_DB_DRIVER"
	EnvDSN         = "QUETZAL_DB_DSN"
	EnvHost        = "QUETZAL_DB_HOST"
	EnvPort        = "QUETZAL_DB_PORT"
	EnvName        = "QUETZAL_DB_NAME"
	EnvUser        = "QUETZAL_DB_USER"
	EnvPassword    = "QUETZAL_DB_PASSWORD"
	EnvSSLMode     = "QUETZAL_DB_SSLMODE"
	EnvSSLRootCert = "QUETZAL_DB_SSLROOTCERT"
)

// postgresFields are the settings that give a PostgreSQL connection field by
// field, as an alternative to a DSN.
var postgresFields = []string{EnvHost, EnvPort, EnvName, EnvUser, EnvPassword, EnvSSLMode, EnvSSLRootCert}

// sslModes are libpq's, which pgx follows.
var sslModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true,
	"require": true, "verify-ca": true, "verify-full": true,
}

// connectTimeout bounds how long opening a connection may take, in seconds.
// Without one a server that drops packets holds a starting pod for the
// system's TCP timeout, two minutes, before it says anything.
const connectTimeout = "10"

// DatabaseFromEnv works out the database to open from the environment:
// QUETZAL_DB_DRIVER, then QUETZAL_DB_DSN or, for PostgreSQL, the connection
// field by field -- QUETZAL_DB_HOST, and where the defaults do not fit
// QUETZAL_DB_PORT (5432), _NAME (quetzal), _USER (quetzal), _PASSWORD,
// _SSLMODE and _SSLROOTCERT. Fields spare the escaping a DSN needs: a password
// with an @, a : or a / broke one written by hand. Given both, it refuses
// rather than pick one. app names the process to the server
// (application_name), so a database's administrator can tell the panel's
// connections from the others'.
func DatabaseFromEnv(getenv func(string) string, app string) (Driver, string, error) {
	driver := Driver(getenv(EnvDriver))
	if driver == "" {
		driver = DriverSQLite
	}
	dsn := getenv(EnvDSN)
	var fields []string
	for _, k := range postgresFields {
		if getenv(k) != "" {
			fields = append(fields, k)
		}
	}
	switch driver {
	case DriverSQLite:
		if len(fields) > 0 {
			return "", "", fmt.Errorf("%s is a PostgreSQL setting, and %s is %q", fields[0], EnvDriver, driver)
		}
		if dsn == "" {
			dsn = "quetzal.db"
		}
		return driver, dsn, nil
	case DriverPostgres:
	default:
		return "", "", fmt.Errorf("%s=%q: the drivers are %q and %q", EnvDriver, driver, DriverSQLite, DriverPostgres)
	}
	if dsn != "" {
		if len(fields) > 0 {
			return "", "", fmt.Errorf("set %s or %s and the fields that go with it, not both (%s is set too)", EnvDSN, EnvHost, fields[0])
		}
		return driver, dsn, nil
	}
	host := getenv(EnvHost)
	if host == "" {
		return "", "", errors.New("PostgreSQL needs " + EnvHost + " (or a full DSN in " + EnvDSN + ")")
	}
	port := or(getenv(EnvPort), "5432")
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("%s=%q is not a port", EnvPort, port)
	}
	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + or(getenv(EnvName), "quetzal"),
	}
	user := or(getenv(EnvUser), "quetzal")
	if pw := getenv(EnvPassword); pw != "" {
		u.User = url.UserPassword(user, pw)
	} else {
		u.User = url.User(user)
	}
	q := url.Values{}
	if mode := getenv(EnvSSLMode); mode != "" {
		if !sslModes[mode] {
			return "", "", fmt.Errorf("%s=%q: one of disable, allow, prefer, require, verify-ca, verify-full", EnvSSLMode, mode)
		}
		q.Set("sslmode", mode)
	}
	if ca := getenv(EnvSSLRootCert); ca != "" {
		q.Set("sslrootcert", ca)
	}
	q.Set("connect_timeout", connectTimeout)
	if app != "" {
		q.Set("application_name", app)
	}
	u.RawQuery = q.Encode()
	return driver, u.String(), nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
