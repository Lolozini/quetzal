package fileops

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingArchiveDoesNotStartSuccessfulStream(t *testing.T) {
	root := t.TempDir()
	var out, stderr bytes.Buffer
	code := Run([]string{root, "archive", filepath.Join(root, "missing")}, nil, &out, &stderr)
	if code != 6 || out.Len() != 0 {
		t.Fatalf("missing archive: code=%d bytes=%d error=%s", code, out.Len(), stderr.String())
	}
}

func TestMalformedArchivesKeepOperationFailureCode(t *testing.T) {
	for _, format := range []string{"zip", "tar", "z"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			var out, stderr bytes.Buffer
			code := Run([]string{root, "extract", root, format}, strings.NewReader("not an archive"), &out, &stderr)
			if code != 1 {
				t.Fatalf("invalid archive: code=%d error=%s", code, stderr.String())
			}
		})
	}
}
