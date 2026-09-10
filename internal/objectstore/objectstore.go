// Package objectstore does the one thing Quetzal needs from S3 directly:
// confirm a backup target exists and answers to the credentials it was given.
//
// Everything else goes through restic in a Job, deliberately — but restic
// creates the bucket when it is missing, so a typo in the bucket name does not
// fail, it quietly starts a second bucket and sends the backups there. The
// mistake then looks exactly like success until someone goes looking for the
// data. Checking at the point the target is configured is the only place the
// difference between "this bucket" and "a bucket I just made up" is still known.
//
// It signs its own request rather than pulling in an SDK: one GET, one
// signature, against a protocol that has not changed in a decade.
package objectstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Target is a bucket and the credentials to reach it.
type Target struct {
	Endpoint string // host, optionally with a port; no scheme
	Region   string
	Bucket   string
	UseSSL   bool
	Access   string
	Secret   string
}

// ErrIndeterminate reports that the target could not be reached at all, as
// opposed to answering that it is wrong. A caller validating user input should
// let an indeterminate result through: the panel failing to reach the object
// store does not mean the backup Jobs will, and refusing the configuration
// would leave the operator unable to set up backups over a transient fault.
var ErrIndeterminate = errors.New("could not reach the object store")

// emptyPayload is the SHA-256 every request here signs over, computed rather
// than written out: a signature built on a mistyped constant fails in a way
// that looks exactly like bad credentials.
var emptyPayload = sha256hex("")

// CheckBucket reports whether the bucket exists and the credentials can read it.
// A missing bucket or a rejected credential comes back as a plain error; being
// unable to ask at all wraps ErrIndeterminate.
func CheckBucket(ctx context.Context, t Target, client *http.Client) error {
	if strings.TrimSpace(t.Endpoint) == "" || strings.TrimSpace(t.Bucket) == "" {
		return errors.New("endpoint and bucket are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	// Virtual-hosted style first (AWS, Scaleway, most providers), then
	// path-style, which MinIO and some gateways serve instead. Only the second
	// answer decides: a provider that does not do virtual-hosted buckets returns
	// its own error for the first, which says nothing about the bucket.
	err := probe(ctx, t, client, false)
	if err == nil {
		return nil
	}
	if pathErr := probe(ctx, t, client, true); pathErr == nil {
		return nil
	} else if errors.Is(err, ErrIndeterminate) {
		return pathErr
	}
	return err
}

func probe(ctx context.Context, t Target, client *http.Client, pathStyle bool) error {
	scheme := "http"
	if t.UseSSL {
		scheme = "https"
	}
	host, path := t.Endpoint, "/"
	if pathStyle {
		path = "/" + t.Bucket
	} else {
		host = t.Bucket + "." + t.Endpoint
	}
	query := "list-type=2&max-keys=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		scheme+"://"+host+path+"?"+query, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIndeterminate, err)
	}
	sign(req, t, host, path, query)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIndeterminate, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("bucket %q does not exist at %s — create it first (Quetzal will not create it for you)", t.Bucket, t.Endpoint)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("the credentials were refused for bucket %q (they need to list, read, write and delete objects)", t.Bucket)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %s returned %d", ErrIndeterminate, t.Endpoint, resp.StatusCode)
	default:
		return fmt.Errorf("%s returned %d: %s", t.Endpoint, resp.StatusCode, summarize(body))
	}
}

// summarize pulls the message out of an S3 error document, falling back to the
// raw body. The body is provider-controlled, so it is trimmed to something that
// fits in an error message.
func summarize(body []byte) string {
	s := string(body)
	if i := strings.Index(s, "<Message>"); i >= 0 {
		if j := strings.Index(s[i:], "</Message>"); j > 0 {
			s = s[i+len("<Message>") : i+j]
		}
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// sign adds an AWS Signature Version 4 for an empty-payload GET.
func sign(req *http.Request, t Target, host, path, query string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	region := t.Region
	if region == "" {
		region = "us-east-1"
	}

	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", emptyPayload)

	canonical := strings.Join([]string{
		http.MethodGet,
		uriEncodePath(path),
		query,
		"host:" + host + "\n" +
			"x-amz-content-sha256:" + emptyPayload + "\n" +
			"x-amz-date:" + amzDate + "\n",
		"host;x-amz-content-sha256;x-amz-date",
		emptyPayload,
	}, "\n")

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	toSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256hex(canonical),
	}, "\n")

	key := []byte("AWS4" + t.Secret)
	for _, part := range []string{dateStamp, region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	sig := hex.EncodeToString(hmacSHA256(key, toSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+t.Access+"/"+scope+
		", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+sig)
}

// uriEncodePath encodes a path the way SigV4 wants it: each segment escaped,
// slashes left alone.
func uriEncodePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(url.QueryEscape(part), "+", "%20")
	}
	return strings.Join(parts, "/")
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
