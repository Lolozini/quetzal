package notify

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/safefetch"
	"golang.org/x/net/dns/dnsmessage"
)

// A channel-owned SMTP endpoint is untrusted even when it names the same
// internal relay an administrator deliberately configured for panel mail.
func TestDeliverToRejectsTenantSMTPOnLoopback(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "::ffff:127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			bind := "127.0.0.1:0"
			if host == "::1" {
				bind = "[::1]:0"
			}
			ln, err := net.Listen("tcp", bind)
			if err != nil {
				if host == "::1" {
					t.Skipf("IPv6 loopback unavailable: %v", err)
				}
				t.Fatal(err)
			}
			defer ln.Close()
			connected := make(chan bool, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					connected <- false
					return
				}
				conn.Close()
				connected <- true
			}()
			_, port, err := net.SplitHostPort(ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			cfg := map[string]string{
				"host": host, "port": port, "tls": "none",
				"from": "panel@example.test", "to": "ops@example.test",
			}
			d := New(&fakeStore{smtp: cfg})
			channel := &models.NotificationChannel{ID: 1, ServerID: 42, Type: models.ChannelEmail, Enabled: true}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = d.DeliverTo(ctx, channel, cfg, models.Event{Type: models.EventServerCrashed})
			ln.Close()
			if <-connected {
				t.Error("tenant SMTP reached the internal listener")
			}
			if err == nil || !strings.Contains(err.Error(), safefetch.ErrBlocked.Error()) {
				t.Errorf("delivery error = %v, want non-public destination refusal", err)
			}
		})
	}
}

func TestDeliverToRejectsTenantSMTPPrivateDestinations(t *testing.T) {
	d := New(&fakeStore{})
	channel := &models.NotificationChannel{ID: 1, ServerID: 42, Type: models.ChannelEmail, Enabled: true}
	for _, host := range []string{
		"10.1.2.3", "172.16.5.5", "192.168.0.1", "169.254.169.254",
		"100.64.0.1", "fc00::1", "fe80::1", "::ffff:10.0.0.1", "64:ff9b::7f00:1",
	} {
		t.Run(host, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cfg := map[string]string{
				"host": host, "port": "25", "tls": "none",
				"from": "panel@example.test", "to": "ops@example.test",
			}
			err := d.DeliverTo(ctx, channel, cfg, models.Event{Type: models.EventServerCrashed})
			if err == nil || !strings.Contains(err.Error(), safefetch.ErrBlocked.Error()) {
				t.Errorf("delivery to %s = %v, want non-public destination refusal", host, err)
			}
		})
	}
}

func TestDeliverToRejectsTenantSMTPAfterDNSRebinding(t *testing.T) {
	dns, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var rebound atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, addr, err := dns.ReadFrom(buf)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if query.Unpack(buf[:n]) != nil {
				continue
			}
			reply := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: true, RecursionAvailable: true},
				Questions: query.Questions,
			}
			for _, q := range query.Questions {
				if q.Type != dnsmessage.TypeA {
					continue
				}
				ip := [4]byte{8, 8, 8, 8}
				if rebound.Load() {
					ip = [4]byte{127, 0, 0, 1}
				}
				reply.Answers = append(reply.Answers, dnsmessage.Resource{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
					Body:   &dnsmessage.AResource{A: ip},
				})
			}
			wire, err := reply.Pack()
			if err != nil {
				continue
			}
			_, _ = dns.WriteTo(wire, addr)
		}
	}()
	defer func() {
		dns.Close()
		<-done
	}()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", dns.LocalAddr().String())
		},
	}
	old := net.DefaultResolver
	net.DefaultResolver = resolver
	defer func() { net.DefaultResolver = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const host = "smtp.rebind.test."
	ips, err := resolver.LookupIP(ctx, "ip4", host)
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv4(8, 8, 8, 8)) {
		t.Fatalf("initial public resolution = %v, %v", ips, err)
	}
	rebound.Store(true)
	_, port, got := fakeSMTP(t)
	cfg := map[string]string{
		"host": host, "port": port, "tls": "none",
		"from": "panel@example.test", "to": "ops@example.test",
	}
	d := New(&fakeStore{})
	channel := &models.NotificationChannel{ID: 1, ServerID: 42, Type: models.ChannelEmail, Enabled: true}
	err = d.DeliverTo(ctx, channel, cfg, models.Event{Type: models.EventServerCrashed})
	if err == nil {
		t.Fatalf("tenant SMTP delivered to the rebound internal relay: %v", <-got)
	}
	if !strings.Contains(err.Error(), safefetch.ErrBlocked.Error()) {
		t.Errorf("delivery error = %v, want non-public destination refusal", err)
	}
}
