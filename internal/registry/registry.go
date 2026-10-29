// Package registry finds out what a tag of a container image points at now, by
// asking its registry for the manifest's digest the way a kubelet would before
// pulling, without pulling anything. The controller pins a game's pod to that
// digest when the game starts: a tag a node had already pulled was never pulled
// again, so an image fixed upstream reached no server that had run the old one.
//
// It reads public images only, anonymously, and connects to public addresses
// only (safefetch): the registry names the server that hands out its tokens,
// and that is not a reason to call into the cluster's network.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/safefetch"
)

// manifestTypes are what a kubelet accepts, an index first: the digest of a
// multi-arch image is its index's, which every node resolves to its own image.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Resolver looks digests up. The zero value is ready to use.
type Resolver struct {
	// Client makes the requests; nil uses one that connects to public
	// addresses only.
	Client *http.Client
}

func (r *Resolver) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{
		Timeout:       15 * time.Second,
		Transport:     safefetch.SafeTransport(),
		CheckRedirect: safefetch.CheckRedirect,
	}
}

// Digest returns the digest ref's tag points at, "sha256:…". A ref that is
// already a digest is not one to look up.
func (r *Resolver) Digest(ctx context.Context, ref string) (string, error) {
	host, repo, tag, err := parse(ref)
	if err != nil {
		return "", err
	}
	manifest := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, tag)
	resp, err := r.head(ctx, manifest, "")
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		token, terr := r.token(ctx, resp.Header.Get("WWW-Authenticate"), repo)
		if terr != nil {
			return "", fmt.Errorf("%s: %w", ref, terr)
		}
		if resp, err = r.head(ctx, manifest, token); err != nil {
			return "", err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: the registry answered HTTP %d", ref, resp.StatusCode)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if !digestRe.MatchString(d) {
		return "", fmt.Errorf("%s: the registry gave no sha256 digest", ref)
	}
	return d, nil
}

func (r *Resolver) head(ctx context.Context, rawURL, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestTypes)
	req.Header.Set("User-Agent", "quetzal")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}

// token asks the server a 401 named for an anonymous pull token, as Docker Hub
// and GHCR want even for a public image.
func (r *Resolver) token(ctx context.Context, challenge, repo string) (string, error) {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") || params["realm"] == "" {
		return "", errors.New("the registry wants credentials")
	}
	u, err := url.Parse(params["realm"])
	if err != nil || u.Scheme != "https" {
		return "", fmt.Errorf("unusable token realm %q", params["realm"])
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "quetzal")
	resp, err := r.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", errors.New("token: none given")
}

// parse splits an image reference as a container runtime reads it: a first
// component with a dot or a colon in it, or "localhost", is the registry, and
// anything else is on Docker Hub, an official image under library/.
func parse(ref string) (host, repo, tag string, err error) {
	if ref == "" || strings.ContainsAny(ref, "@ \t\n") {
		return "", "", "", fmt.Errorf("%q is not a tag to look up", ref)
	}
	name := ref
	tag = "latest"
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i+1:]
	}
	host = "docker.io"
	if i := strings.Index(name, "/"); i >= 0 {
		first := name[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host, name = first, name[i+1:]
		}
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(name, "/") {
			name = "library/" + name
		}
	}
	if name == "" || tag == "" {
		return "", "", "", fmt.Errorf("%q is not a tag to look up", ref)
	}
	return host, strings.ToLower(name), tag, nil
}

// parseChallenge reads a WWW-Authenticate header: a scheme, then key="value"
// pairs, values quoted and possibly holding commas.
func parseChallenge(h string) (string, map[string]string) {
	h = strings.TrimSpace(h)
	scheme, rest, _ := strings.Cut(h, " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			val, rest = after[1:1+end], after[2+end:]
		} else {
			val, rest, _ = strings.Cut(after, ",")
			val = strings.TrimSpace(val)
		}
		params[key] = val
		rest = strings.TrimLeft(strings.TrimSpace(rest), ",")
		rest = strings.TrimSpace(rest)
	}
	return scheme, params
}
