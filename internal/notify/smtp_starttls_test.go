package notify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSMTP serves one SMTP conversation on loopback, advertising exts after
// EHLO, and reports every command line it received once the client is done,
// and the message's lines after a "| ". Like a real relay, it refuses a MAIL
// or RCPT whose argument is anything but <address>.
// It never speaks TLS: the point is what a client does when TLS is not on offer
// -- which is exactly what an attacker who strips STARTTLS from the EHLO reply
// arranges.
func fakeSMTP(t *testing.T, exts ...string) (host, port string, got <-chan []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan []string, 1)
	go func() {
		var lines []string
		defer func() { ch <- lines }()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		inData := false
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			l = strings.TrimRight(l, "\r\n")
			if inData {
				if l == "." {
					inData = false
					say("250 queued")
				} else {
					lines = append(lines, "| "+l)
				}
				continue
			}
			lines = append(lines, l)
			switch cmd := strings.ToUpper(strings.SplitN(l, " ", 2)[0]); cmd {
			case "EHLO", "HELO":
				say("250-fake")
				for _, e := range exts {
					say("250-" + e)
				}
				say("250 8BITMIME")
			case "STARTTLS":
				say("454 TLS not available")
			case "AUTH":
				say("235 ok")
			case "MAIL", "RCPT":
				arg := strings.SplitN(strings.TrimSpace(l[strings.Index(l, ":")+1:]), " ", 2)[0]
				if !strings.HasPrefix(arg, "<") || !strings.HasSuffix(arg, ">") || strings.ContainsAny(arg[1:len(arg)-1], "<> ") {
					say("501 5.5.4 invalid " + cmd + " parameter")
					continue
				}
				say("250 ok")
			case "DATA":
				inData = true
				say("354 go ahead")
			case "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p, ch
}

func sent(lines []string, verb string) bool {
	for _, l := range lines {
		if strings.HasPrefix(strings.ToUpper(l), verb) {
			return true
		}
	}
	return false
}

func sendVia(host, port, mode string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return SendMail(ctx, map[string]string{
		"host": host, "port": port, "from": "panel@example.test", "tls": mode,
		// Credentials on purpose: loopback is the one place net/smtp would hand
		// them over without TLS, so nothing but our own check stands in the way.
		"username": "relay-user", "password": "relay-secret",
	}, []string{"admin@example.test"}, "Reset your password", "https://panel.example/reset?token=SECRET")
}

// "STARTTLS" is what the panel's form calls this mode, and what it selects by
// default. It used to mean "if the server offers it": a relay that does not --
// or a man in the middle who deletes the offer from the EHLO reply -- got the
// message in cleartext. The messages this panel sends include password reset
// links, so that is an account takeover for whoever is on the path.
func TestStartTLSModeRefusesARelayThatDoesNotOfferIt(t *testing.T) {
	// An unset mode is what the form displays as STARTTLS, so it must behave as
	// STARTTLS too.
	for _, mode := range []string{"starttls", "", "STARTTLS"} {
		t.Run("mode="+mode, func(t *testing.T) {
			host, port, got := fakeSMTP(t) // no STARTTLS on offer
			err := sendVia(host, port, mode)
			if err == nil {
				t.Fatal("sent over a connection that never became encrypted")
			}
			if !strings.Contains(err.Error(), "STARTTLS") {
				t.Errorf("the error does not say what was missing: %v", err)
			}
			lines := <-got
			for _, verb := range []string{"AUTH", "MAIL", "RCPT", "DATA"} {
				if sent(lines, verb) {
					t.Errorf("%s went out in cleartext; conversation: %q", verb, lines)
				}
			}
		})
	}
}

// When STARTTLS is on offer it is attempted, and a failure to negotiate it stops
// the send rather than falling back.
func TestStartTLSModeDoesNotFallBackWhenNegotiationFails(t *testing.T) {
	host, port, got := fakeSMTP(t, "STARTTLS")
	if err := sendVia(host, port, "starttls"); err == nil {
		t.Fatal("a failed STARTTLS fell back to cleartext")
	}
	lines := <-got
	if !sent(lines, "STARTTLS") {
		t.Errorf("STARTTLS was on offer and never tried; conversation: %q", lines)
	}
	if sent(lines, "DATA") {
		t.Errorf("the message went out after STARTTLS failed; conversation: %q", lines)
	}
}

// Cleartext stays available, but only when asked for by name.
func TestNoneModeStillSendsInCleartext(t *testing.T) {
	host, port, got := fakeSMTP(t)
	if err := sendVia(host, port, "none"); err != nil {
		t.Fatalf("explicit cleartext refused: %v", err)
	}
	if lines := <-got; !sent(lines, "DATA") {
		t.Errorf("nothing was sent; conversation: %q", lines)
	}
}

// A recipient the relay refuses is told apart from a relay that failed: the
// panel blamed its email settings for an address the server would not take
// (recette of 0.10.0, R-28).
func TestARefusedRecipientIsTheAddresss(t *testing.T) {
	host, port, got := fakeSMTP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := SendMail(ctx, map[string]string{"host": host, "port": port, "from": "panel@example.test", "tls": "none"},
		[]string{"not an address"}, "Invitation", "hello")
	<-got
	var rcpt *RecipientError
	if !errors.As(err, &rcpt) || rcpt.Addr != "not an address" {
		t.Fatalf("err = %v, want a refused recipient", err)
	}
	var perm permanentError
	if !errors.As(err, &perm) {
		t.Errorf("a 5xx refusal is retried: %v", err)
	}
}
