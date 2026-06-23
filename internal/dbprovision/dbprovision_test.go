package dbprovision

import (
	"context"
	"strings"
	"testing"
)

func TestValidIdentRejectsInjection(t *testing.T) {
	good := []string{"s1_abc", "u12_AbC123", "db_name_64"}
	for _, s := range good {
		if !validIdent(s) {
			t.Errorf("validIdent(%q) = false, want true", s)
		}
	}
	bad := []string{
		"", "with space", "back`tick", "quote'", "semi;colon", "dash-no",
		"db`; DROP DATABASE x;--", strings.Repeat("a", 65),
	}
	for _, s := range bad {
		if validIdent(s) {
			t.Errorf("validIdent(%q) = true, want false", s)
		}
	}
}

func TestValidRemoteAndPassword(t *testing.T) {
	for _, s := range []string{"%", "10.0.%", "host.internal", "::1"} {
		if !validRemote(s) {
			t.Errorf("validRemote(%q) = false", s)
		}
	}
	for _, s := range []string{"'", "a b", "ev`il", "x;y", ""} {
		if validRemote(s) {
			t.Errorf("validRemote(%q) = true, want false", s)
		}
	}
	if !validPassword("Abc12345") {
		t.Error("alnum 8-char password should be valid")
	}
	for _, s := range []string{"short", "has space 0000", "quote'0000", "back`tick0"} {
		if validPassword(s) {
			t.Errorf("validPassword(%q) = true, want false", s)
		}
	}
}

func TestGeneratorsProduceValidValues(t *testing.T) {
	for i := 0; i < 200; i++ {
		if name := GenerateName("s", 42); !validIdent(name) || !strings.HasPrefix(name, "s42_") {
			t.Fatalf("GenerateName = %q, invalid", name)
		}
		if pw := GeneratePassword(); !validPassword(pw) || len(pw) != 24 {
			t.Fatalf("GeneratePassword = %q, invalid", pw)
		}
	}
}

func TestProvisionRejectsBadNamesBeforeConnecting(t *testing.T) {
	// A bad identifier must be rejected up front, never reaching a connection.
	c := Conn{Host: "127.0.0.1", Port: 1, User: "x", Password: "y"}
	if err := Provision(context.Background(), c, "bad name", "u", "%", GeneratePassword()); err != ErrInvalidName {
		t.Errorf("Provision bad db = %v, want ErrInvalidName", err)
	}
	if err := Provision(context.Background(), c, "okname", "u", "ev'il", GeneratePassword()); err != ErrInvalidRemote {
		t.Errorf("Provision bad remote = %v, want ErrInvalidRemote", err)
	}
	if err := Provision(context.Background(), c, "okname", "u", "%", "short"); err != ErrInvalidPassword {
		t.Errorf("Provision bad password = %v, want ErrInvalidPassword", err)
	}
}
