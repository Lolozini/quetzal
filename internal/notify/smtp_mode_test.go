package notify

import (
	"net"
	"strings"
	"testing"
	"time"
)

// A server on port 465 expects TLS from the first byte, and waits for it in
// silence. Set to STARTTLS, the panel waited for a greeting that never came,
// until the deadline of the whole send, and then said "i/o timeout". It now
// gives up on the greeting sooner, and says which setting to change.
func TestSTARTTLSAgainstAnImplicitTLSPortSaysToChooseTLS(t *testing.T) {
	old := greetingTimeout
	greetingTimeout = 300 * time.Millisecond
	t.Cleanup(func() { greetingTimeout = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		// Accept, and wait for a TLS handshake that never comes.
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	for _, mode := range []string{"starttls", "", "none"} {
		start := time.Now()
		err := sendVia(host, port, mode) // a 5-second deadline for the whole send
		if err == nil {
			t.Fatalf("mode %q: sent to a server that never spoke", mode)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("mode %q: gave up after %s, want about the greeting timeout", mode, took)
		}
		if !strings.Contains(err.Error(), `choose "tls"`) || !strings.Contains(err.Error(), "465") {
			t.Errorf("mode %q: the error does not say what to change: %v", mode, err)
		}
	}
}

// The other way round: set to TLS against a server that greets in plain text
// (port 587, which wants STARTTLS).
func TestTLSAgainstAPlainTextPortSaysToChooseSTARTTLS(t *testing.T) {
	host, port, _ := fakeSMTP(t, "STARTTLS")
	err := sendVia(host, port, "tls")
	if err == nil {
		t.Fatal("a TLS handshake succeeded against a plain-text server")
	}
	if !strings.Contains(err.Error(), `choose "starttls"`) {
		t.Errorf("the error does not say what to change: %v", err)
	}
}
