package notify

import (
	"context"
	"strings"
	"testing"
	"time"
)

func sendFrom(t *testing.T, from string) ([]string, error) {
	t.Helper()
	host, port, got := fakeSMTP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := SendMail(ctx, map[string]string{"host": host, "port": port, "from": from, "tls": "none"},
		[]string{"admin@example.test"}, "Reset your password", "https://panel.example/reset")
	if err != nil {
		return nil, err
	}
	return <-got, nil
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// A sender written as people write one, "Quetzal <quetzal@example.com>", was
// saved without a word, then refused by the relay on every send, password
// resets included: the whole string went into the envelope. The envelope takes
// the address, and the From header keeps the name.
func TestASenderWithANameSends(t *testing.T) {
	lines, err := sendFrom(t, "Quetzal QA <qz@example.test>")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !has(lines, "MAIL FROM:<qz@example.test> BODY=8BITMIME") {
		t.Errorf("the envelope does not carry the address alone: %q", lines)
	}
	if !has(lines, `| From: "Quetzal QA" <qz@example.test>`) {
		t.Errorf("the From header lost the name: %q", lines)
	}

	// A bare address is sent as it always was.
	lines, err = sendFrom(t, "qz@example.test")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !has(lines, "| From: qz@example.test") {
		t.Errorf("a bare address changed: %q", lines)
	}
}

// A name that is not ASCII goes into the header encoded: net/smtp never asks
// for SMTPUTF8, and a strict relay rejects raw 8-bit headers.
func TestASenderNameThatIsNotASCIIIsEncoded(t *testing.T) {
	lines, err := sendFrom(t, "Équipe Quetzal <qz@example.test>")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "| From: ") {
			if l != "| From: =?utf-8?q?=C3=89quipe_Quetzal?= <qz@example.test>" {
				t.Errorf("From header %q", l)
			}
			return
		}
	}
	t.Errorf("no From header: %q", lines)
}

func TestParseFrom(t *testing.T) {
	for _, ok := range []string{"qz@example.test", "Quetzal <qz@example.test>", `"Quetzal, QA" <qz@example.test>`, " qz@example.test "} {
		if _, err := ParseFrom(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Quetzal", "qz@", "<qz@example.test", "a@example.test, b@example.test"} {
		if _, err := ParseFrom(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
