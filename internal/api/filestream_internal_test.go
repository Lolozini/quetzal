package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Before the first byte is written, a failure is still an ordinary error
// response — and it must not keep the attachment headers set in advance for a
// download that is not coming.
func TestStreamFailedBeforeAnyBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/gzip")
	rec.Header().Set("Content-Disposition", `attachment; filename="world.tar.gz"`)

	streamFailed(rec, "archive failed", 0, errors.New("boom"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q on an error response", got)
	}
	if !strings.Contains(rec.Body.String(), "archive failed") {
		t.Errorf("body does not carry the error: %q", rec.Body.String())
	}
}

// Once bytes are on the wire the status line already said 200. Appending a JSON
// error there hands the caller a truncated file that claims to be complete,
// which is how a half-downloaded world passed for a whole one. The response has
// to be aborted instead, so the transfer breaks and the client notices.
func TestStreamFailedMidStreamAborts(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	rec.Body.WriteString("the first megabyte")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a failure mid-download did not abort the response")
		}
		if r != http.ErrAbortHandler {
			t.Fatalf("panicked with %v, want http.ErrAbortHandler", r)
		}
		if strings.Contains(rec.Body.String(), "error") {
			t.Errorf("an error was appended to the download: %q", rec.Body.String())
		}
	}()
	streamFailed(rec, "archive failed", 18, errors.New("boom"))
}

// countingWriter is what tells those two cases apart, so it has to agree with
// what actually reached the writer.
func TestCountingWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &countingWriter{w: rec}
	for _, chunk := range []string{"abc", "", "defgh"} {
		if _, err := cw.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if cw.n != 8 {
		t.Errorf("counted %d bytes, want 8", cw.n)
	}
	if rec.Body.String() != "abcdefgh" {
		t.Errorf("body = %q", rec.Body.String())
	}
}
