package store

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDatabaseFromEnvRefusesWhatItCannotUse(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"a field on SQLite":       {EnvHost: "db"},
		"an unknown driver":       {EnvDriver: "mysql"},
		"a DSN and a field":       {EnvDriver: "postgres", EnvDSN: "postgres://x@db/q", EnvPassword: "pw"},
		"PostgreSQL with no host": {EnvDriver: "postgres", EnvUser: "quetzal"},
		"PostgreSQL with nothing": {EnvDriver: "postgres"},
		"a port that is not one":  {EnvDriver: "postgres", EnvHost: "db", EnvPort: "543x"},
		"a port out of range":     {EnvDriver: "postgres", EnvHost: "db", EnvPort: "70000"},
		"an sslmode libpq lacks":  {EnvDriver: "postgres", EnvHost: "db", EnvSSLMode: "on"},
	} {
		if _, _, err := DatabaseFromEnv(envOf(env), "test"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDatabaseFromEnvKeepsADSN(t *testing.T) {
	for _, tc := range []struct {
		env    map[string]string
		driver Driver
		dsn    string
	}{
		{map[string]string{}, DriverSQLite, "quetzal.db"},
		{map[string]string{EnvDSN: "/data/q.db"}, DriverSQLite, "/data/q.db"},
		{map[string]string{EnvDriver: "postgres", EnvDSN: "postgres://a:b@db/q"}, DriverPostgres, "postgres://a:b@db/q"},
	} {
		d, dsn, err := DatabaseFromEnv(envOf(tc.env), "test")
		if err != nil || d != tc.driver || dsn != tc.dsn {
			t.Errorf("%v: %q %q %v, want %q %q", tc.env, d, dsn, err, tc.driver, tc.dsn)
		}
	}
}

// What the fields build is what PostgreSQL's driver reads back: a password
// with every character a DSN written by hand trips on, defaults where nothing
// is said, and the process's name.
func TestDatabaseFromEnvBuildsTheConnection(t *testing.T) {
	pw := `p@ss:w/rd?#% &é"`
	_, dsn, err := DatabaseFromEnv(envOf(map[string]string{
		EnvDriver: "postgres", EnvHost: "postgres.postgres.svc.cluster.local", EnvPassword: pw, EnvSSLMode: "disable",
	}), "quetzal-apiserver")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgx cannot read %q: %v", dsn, err)
	}
	if cfg.Host != "postgres.postgres.svc.cluster.local" || cfg.Port != 5432 || cfg.Database != "quetzal" ||
		cfg.User != "quetzal" || cfg.Password != pw || cfg.TLSConfig != nil ||
		cfg.RuntimeParams["application_name"] != "quetzal-apiserver" || cfg.ConnectTimeout != 10*time.Second {
		t.Errorf("read back: host %q port %d db %q user %q password %q tls %v app %q timeout %v",
			cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password, cfg.TLSConfig != nil,
			cfg.RuntimeParams["application_name"], cfg.ConnectTimeout)
	}

	_, dsn, err = DatabaseFromEnv(envOf(map[string]string{
		EnvDriver: "postgres", EnvHost: "::1", EnvPort: "6432", EnvName: "panel", EnvUser: "admin",
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = pgconn.ParseConfig(dsn); err != nil || cfg.Host != "::1" || cfg.Port != 6432 ||
		cfg.Database != "panel" || cfg.User != "admin" || cfg.Password != "" {
		t.Errorf("IPv6, no password: %q -> %+v %v", dsn, cfg, err)
	}
}

// verify-full with a CA: the driver checks the server's certificate against
// it, and its name against the host.
func TestDatabaseFromEnvVerifiesTheServer(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, selfSignedCA(t), 0o600); err != nil {
		t.Fatal(err)
	}
	_, dsn, err := DatabaseFromEnv(envOf(map[string]string{
		EnvDriver: "postgres", EnvHost: "db.example.net", EnvSSLMode: "verify-full", EnvSSLRootCert: ca,
	}), "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgx cannot read %q: %v", dsn, err)
	}
	if cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.ServerName != "db.example.net" ||
		cfg.TLSConfig.RootCAs == nil || len(cfg.Fallbacks) != 0 {
		t.Errorf("verify-full: tls %+v, fallbacks %d", cfg.TLSConfig, len(cfg.Fallbacks))
	}
	if !strings.Contains(dsn, "sslmode=verify-full") {
		t.Errorf("dsn %q", dsn)
	}
}

func selfSignedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
