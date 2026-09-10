package objectstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func target(endpoint string) Target {
	return Target{
		Endpoint: endpoint, Region: "fr-par", Bucket: "backups",
		Access: "AK123", Secret: "sk-secret",
	}
}

// The distinction that matters is between "the target is wrong" and "I could not
// ask": the first has to stop an operator saving a typo, the second must not
// stop them configuring backups because the panel briefly could not reach the
// object store.
func TestCheckBucketSeparatesWrongFromUnreachable(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantErr       bool
		indeterminate bool
		contains      string
	}{
		{"bucket exists", http.StatusOK, "<ListBucketResult/>", false, false, ""},
		{"no such bucket", http.StatusNotFound, "<Error><Code>NoSuchBucket</Code></Error>", true, false, "does not exist"},
		{"credentials refused", http.StatusForbidden, "<Error><Code>AccessDenied</Code></Error>", true, false, "credentials were refused"},
		{"provider is down", http.StatusBadGateway, "oops", true, true, "could not reach"},
		{"something else", http.StatusTeapot, "<Error><Message>go away</Message></Error>", true, false, "go away"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			err := CheckBucket(context.Background(), target(strings.TrimPrefix(srv.URL, "http://")), srv.Client())
			if c.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if err == nil {
				return
			}
			if got := errors.Is(err, ErrIndeterminate); got != c.indeterminate {
				t.Errorf("indeterminate = %v, want %v (%v)", got, c.indeterminate, err)
			}
			if !strings.Contains(err.Error(), c.contains) {
				t.Errorf("error %q does not mention %q", err, c.contains)
			}
		})
	}
}

// A provider that does not serve virtual-hosted buckets must not be reported as
// a missing bucket. MinIO and several gateways only answer path-style.
//
// The virtual-hosted attempt does not reach this server at all — "backups." in
// front of a literal address does not resolve — which is exactly the shape of
// the failure the fallback exists for, and stronger than a 404 would be: the
// check has to recover from the first style failing for any reason, not just
// from a tidy status code.
func TestCheckBucketFallsBackToPathStyle(t *testing.T) {
	var pathRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/backups") {
			t.Errorf("unexpected non-path-style request for %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		pathRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := CheckBucket(context.Background(), target(strings.TrimPrefix(srv.URL, "http://")), srv.Client()); err != nil {
		t.Fatalf("path-style bucket reported as missing: %v", err)
	}
	if pathRequests != 1 {
		t.Errorf("path-style requests = %d, want 1", pathRequests)
	}
}

// The request has to be signed, and signed over the payload hash it declares —
// a mistyped constant there fails identically to bad credentials, which is a
// miserable thing to debug.
func TestCheckBucketSignsTheRequest(t *testing.T) {
	var auth, payload, date string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		payload = r.Header.Get("X-Amz-Content-Sha256")
		date = r.Header.Get("X-Amz-Date")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := CheckBucket(context.Background(), target(strings.TrimPrefix(srv.URL, "http://")), srv.Client()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"AWS4-HMAC-SHA256", "Credential=AK123/", "/fr-par/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date", "Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization %q missing %q", auth, want)
		}
	}
	if payload != sha256hex("") {
		t.Errorf("payload hash = %q, want the hash of an empty body", payload)
	}
	if strings.Contains(auth, "sk-secret") {
		t.Error("the secret key itself is in the Authorization header")
	}
	if len(date) != 16 || !strings.HasSuffix(date, "Z") {
		t.Errorf("X-Amz-Date = %q, want a basic-format UTC timestamp", date)
	}
}

func TestCheckBucketRequiresATarget(t *testing.T) {
	if err := CheckBucket(context.Background(), Target{Bucket: "b"}, nil); err == nil {
		t.Error("accepted a target with no endpoint")
	}
	if err := CheckBucket(context.Background(), Target{Endpoint: "e"}, nil); err == nil {
		t.Error("accepted a target with no bucket")
	}
}
