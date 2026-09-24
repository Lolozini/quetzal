// Package safefetch performs outbound HTTP GETs with SSRF protection: it only
// allows http(s), refuses to connect to non-public IPs (loopback, private,
// link-local — including the cloud metadata endpoint 169.254.169.254), bounds
// the response size, and caps redirects. It is used for admin-initiated fetches
// of egg JSON from arbitrary URLs.
package safefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// specialV4 are the IPv4 ranges net.IP's predicates leave out that still never
// lead to the public internet. The one that matters is 100.64.0.0/10: carrier-
// grade NAT, which is also where Tailscale puts its nodes and where some
// clusters (EKS with custom networking, among others) put their pods, so a
// guard that only knew RFC 1918 let a user-supplied URL reach them.
var specialV4 = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8",     // "this network"
		"100.64.0.0/10", // shared address space (RFC 6598): CGNAT, Tailscale, some pod networks
		"192.0.0.0/24",  // IETF protocol assignments
		"198.18.0.0/15", // benchmarking
		"240.0.0.0/4",   // reserved, and the limited broadcast address
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// nat64 is the well-known NAT64 prefix (RFC 6052): an address in it reaches
// the IPv4 address in its last four bytes, so that is the address to judge.
var _, nat64, _ = net.ParseCIDR("64:ff9b::/96")

// nat64Local is the local-use NAT64 prefix (RFC 8215). Where the IPv4 address
// sits in it depends on the operator's prefix length, and it is local by
// definition, so it is refused as a whole.
var _, nat64Local, _ = net.ParseCIDR("64:ff9b:1::/48")

// blockedIP reports whether dialing ip should be refused (non-public ranges).
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() ||
		ip.IsPrivate() || // RFC1918 + fc00::/7 unique-local
		ip.IsLinkLocalUnicast() || // 169.254.0.0/16 (incl. metadata) + fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range specialV4 {
			if n.Contains(v4) {
				return true
			}
		}
		return false
	}
	if nat64.Contains(ip) {
		return blockedIP(net.IP(ip[12:16]))
	}
	return nat64Local.Contains(ip)
}

// ErrBlocked is wrapped into the error for a connection refused because the
// address it would reach is not public. It is a policy decision, not a network
// failure, so a caller that retries can tell the two apart with errors.Is and
// stop trying: the same address will be refused the same way every time.
var ErrBlocked = errors.New("refusing to connect to non-public address")

// guard runs for every actual TCP connection (each resolved IP, each redirect
// hop), so it defeats DNS rebinding: the IP being dialed is what's checked.
func guard(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); blockedIP(ip) {
		return fmt.Errorf("%w %s", ErrBlocked, host)
	}
	return nil
}

// SafeTransport returns an *http.Transport whose dialer refuses connections to
// non-public addresses (see blockedIP). The guard runs for every actual TCP
// connection — each resolved IP and each redirect hop — so it also defeats DNS
// rebinding. Pair it with CheckRedirect on an http.Client for any outbound
// request to a user-supplied URL (egg fetch, notification webhooks).
func SafeTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: guard}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableKeepAlives:     true,
	}
}

// CheckRedirect bounds an http.Client's redirect chain and restricts it to
// http(s). The dialer in SafeTransport re-checks each hop's IP, so this only
// guards the scheme and chain length.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("too many redirects")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to non-http(s) scheme")
	}
	return nil
}

// Get fetches rawURL and returns its body (capped at maxBytes). The total
// operation is bounded by ctx and an internal timeout.
func Get(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL must be http or https")
	}

	client := &http.Client{
		Timeout:       30 * time.Second,
		Transport:     SafeTransport(),
		CheckRedirect: CheckRedirect,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "quetzal-egg-fetch")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch failed: HTTP %d", resp.StatusCode)
	}
	// Read one byte past the cap to detect oversize without trusting Content-Length.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBytes)
	}
	return body, nil
}
