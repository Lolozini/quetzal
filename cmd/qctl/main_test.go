package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/testdb"
)

const cliTestKey = "0123456789abcdef0123456789abcdef"

// Run the real entry point in a child process so fatalf/os.Exit retain their
// production behavior without terminating the test runner.
func TestQctlProcess(t *testing.T) {
	if os.Getenv("QUETZAL_TEST_QCTL_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"qctl"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing command separator")
}

func cliStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := testdb.DSN(t, "qctl.db")
	st, err := store.Open(store.Config{
		Driver: store.Driver(testdb.Driver()), DSN: dsn, Silent: true,
		SecretKey: []byte(cliTestKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st, dsn
}

func runCLI(t *testing.T, dsn string, args ...string) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, append([]string{"-test.run=^TestQctlProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(),
		"QUETZAL_TEST_QCTL_PROCESS=1",
		"QUETZAL_DB_DRIVER="+testdb.Driver(),
		"QUETZAL_DB_DSN="+dsn,
		"QUETZAL_SECRET_KEY="+base64.StdEncoding.EncodeToString([]byte(cliTestKey)),
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestCreateSeparatesSecrets(t *testing.T) {
	st, dsn := cliStore(t)
	tmpl, err := st.UpsertTemplate(&models.Template{
		Slug: "private", Name: "Private",
		Variables: []models.TemplateVariable{
			{EnvVariable: "PASSWORD", Default: "private-default", Secret: true},
			{EnvVariable: "TOKEN", Default: "token-default", Secret: true},
			{EnvVariable: "EMPTY_SECRET", Secret: true},
			{EnvVariable: "PUBLIC", Default: "public-default"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		plain  map[string]string
		secret map[string]string
	}{
		{"defaults", nil, map[string]string{"PUBLIC": "public-default"}, map[string]string{"PASSWORD": "private-default", "TOKEN": "token-default"}},
		{"overrides", []string{"--env", "PASSWORD=discarded", "--env", "PASSWORD=private=override", "--env", "TOKEN=", "--env", "EMPTY_SECRET=filled-secret", "--env", "PUBLIC=changed", "--env", "EXTRA=kept"}, map[string]string{"PUBLIC": "changed", "EXTRA": "kept"}, map[string]string{"PASSWORD": "private=override", "TOKEN": "", "EMPTY_SECRET": "filled-secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"create", "--template", tmpl.Slug, "--name", tc.name}, tc.args...)
			if out, err := runCLI(t, dsn, args...); err != nil {
				t.Fatalf("create: %v: %s", err, out)
			}
			srv, err := st.GetServerBySlug(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(srv.Env, tc.plain) {
				t.Errorf("public environment = %v, want %v", srv.Env, tc.plain)
			}
			if !strings.HasPrefix(srv.SecretEnvEnc, "enc:") {
				t.Error("secrets were not encrypted with the configured key")
			}
			secret, err := st.OpenSecrets(srv.SecretEnvEnc)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(secret, tc.secret) {
				t.Errorf("secret environment = %v, want %v", secret, tc.secret)
			}
			public, err := json.Marshal(srv)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range tc.secret {
				if strings.Contains(string(public), `"`+name+`"`) || (value != "" && strings.Contains(string(public), value)) {
					t.Errorf("serialized server exposes secret %s", name)
				}
			}
		})
	}
}

func TestCreateRejectsMalformedEnvWithoutEchoing(t *testing.T) {
	st, dsn := cliStore(t)
	tmpl, err := st.UpsertTemplate(&models.Template{Slug: "private", Name: "Private"})
	if err != nil {
		t.Fatal(err)
	}
	const input = "mistyped-private-value"
	out, err := runCLI(t, dsn, "create", "--template", tmpl.Slug, "--name", "invalid-env", "--env", input)
	if err == nil {
		t.Fatalf("malformed environment accepted: %s", out)
	}
	if strings.Contains(out, input) {
		t.Error("malformed environment value was echoed to output")
	}
	servers, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Error("malformed environment created a server")
	}
}

func TestSetStateWakesHibernated(t *testing.T) {
	st, dsn := cliStore(t)
	stale := time.Now().Add(-24 * time.Hour)
	srv := &models.Server{Slug: "sleeping", DesiredState: models.StateStopped, Hibernated: true, LastActiveAt: &stale}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if out, err := runCLI(t, dsn, "set-state", "--slug", srv.Slug, "--state", "Running"); err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	got, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredState != models.StateRunning || got.Hibernated || got.Replicas() != 1 {
		t.Errorf("server did not wake: state=%s hibernated=%v replicas=%d", got.DesiredState, got.Hibernated, got.Replicas())
	}
	if got.LastActiveAt == nil || got.LastActiveAt.Before(before.Add(-time.Second)) {
		t.Error("start did not reset the idle timer")
	}
}

func TestSetStateRejectsInvalid(t *testing.T) {
	st, dsn := cliStore(t)
	srv := &models.Server{Slug: "invalid", DesiredState: models.StateStopped, Hibernated: true}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "invalid", ""} {
		if out, err := runCLI(t, dsn, "set-state", "--slug", srv.Slug, "--state", state); err == nil {
			t.Errorf("state %q accepted: %s", state, out)
		}
		got, err := st.GetServer(srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, before) {
			t.Errorf("invalid state %q mutated the server", state)
		}
	}
}

func TestSetStateStopsAndSuspends(t *testing.T) {
	st, dsn := cliStore(t)
	srv := &models.Server{Slug: "running", DesiredState: models.StateRunning, Status: models.Status{Phase: models.PhaseRunning}, Env: map[string]string{"PUBLIC": "kept"}}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	for _, state := range []models.DesiredState{models.StateStopped, models.StateSuspended} {
		if out, err := runCLI(t, dsn, "set-state", "--slug", srv.Slug, "--state", string(state)); err != nil {
			t.Fatalf("set state %s: %v: %s", state, err, out)
		}
		got, err := st.GetServer(srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.DesiredState != state || got.Status.Phase != models.PhaseRunning || got.Env["PUBLIC"] != "kept" {
			t.Errorf("state change did not preserve unrelated fields: %+v", got)
		}
	}
}

func TestSetStateRefusesOfflineOperations(t *testing.T) {
	for _, direction := range []models.BackupDirection{models.DirRestore, models.DirDatabaseImport} {
		for _, phase := range []models.BackupPhase{models.BackupPending, models.BackupRunning} {
			t.Run(string(direction)+"/"+string(phase), func(t *testing.T) {
				st, dsn := cliStore(t)
				srv := &models.Server{Slug: "busy", DesiredState: models.StateStopped, Hibernated: true}
				if err := st.CreateServer(srv); err != nil {
					t.Fatal(err)
				}
				op := &models.Backup{ServerID: srv.ID, Direction: direction, Phase: phase}
				var err error
				if direction == models.DirRestore {
					err = st.CreateRestore(op)
				} else {
					err = st.CreateDatabaseImport(op)
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err := st.GetServer(srv.ID)
				if err != nil {
					t.Fatal(err)
				}
				if out, err := runCLI(t, dsn, "set-state", "--slug", srv.Slug, "--state", "Running"); err == nil {
					t.Errorf("start during %s %s accepted: %s", direction, phase, out)
				}
				got, err := st.GetServer(srv.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, before) {
					t.Error("refused start mutated the server")
				}
			})
		}
	}
}

func TestSetStateRefusesTransfer(t *testing.T) {
	st, dsn := cliStore(t)
	srv := &models.Server{Slug: "moving", DesiredState: models.StateStopped, Transfer: &models.TransferState{Phase: models.TransferBackingUp, SourceCluster: 1, TargetCluster: 2}}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"Running", "Stopped", "Suspended"} {
		if out, err := runCLI(t, dsn, "set-state", "--slug", srv.Slug, "--state", state); err == nil {
			t.Errorf("state %s during transfer accepted: %s", state, out)
		}
		got, err := st.GetServer(srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, before) {
			t.Errorf("refused state %s mutated the server", state)
		}
	}
}
