package fileops

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type archiveTestEntry struct {
	name string
	body string
	link string
	kind byte
	mode int64
}

var archiveTestTime = time.Unix(1700000000, 0)

func archiveTestBytes(t *testing.T, format string, entries ...archiveTestEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	if format == "zip" {
		zw := zip.NewWriter(&out)
		for _, entry := range entries {
			h := &zip.FileHeader{Name: entry.name, Method: zip.Deflate, Modified: archiveTestTime}
			mode := os.FileMode(entry.mode)
			switch entry.kind {
			case tar.TypeDir:
				mode |= os.ModeDir
				if !strings.HasSuffix(h.Name, "/") {
					h.Name += "/"
				}
			case tar.TypeSymlink:
				mode |= os.ModeSymlink
			case tar.TypeFifo:
				mode |= os.ModeNamedPipe
			}
			h.SetMode(mode)
			w, err := zw.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			body := entry.body
			if entry.kind == tar.TypeSymlink {
				body = entry.link
			}
			if _, err = io.WriteString(w, body); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		tw := tar.NewWriter(&out)
		for _, entry := range entries {
			kind := entry.kind
			if kind == 0 {
				kind = tar.TypeReg
			}
			h := &tar.Header{Name: entry.name, Typeflag: kind, Mode: entry.mode, ModTime: archiveTestTime, Linkname: entry.link}
			if kind == tar.TypeReg {
				h.Size = int64(len(entry.body))
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if h.Size != 0 {
				if _, err := io.WriteString(tw, entry.body); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out.Bytes()
}

func archiveTestRun(t *testing.T, root string, in io.Reader, args ...string) (int, []byte, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Run(append([]string{root}, args...), in, &out, &stderr)
	return code, out.Bytes(), stderr.String()
}

func archiveTestRead(t *testing.T, name string, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s: got %q, want %q", name, got, want)
	}
}

func TestArchiveExtractBenignFormats(t *testing.T) {
	plain := archiveTestBytes(t, "tar", archiveTestEntry{name: "hello.txt", body: "hello world\n", mode: 0640})
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	// Fixed fixtures contain the same file, mode, and timestamp. No external
	// compressor is needed to exercise the production bzip2/xz readers.
	bz, err := base64.StdEncoding.DecodeString("QlpoOTFBWSZTWZ4TBdEAAHR7gMoQAQBAAXeAAIBmRJ7ACAggAHUNSPU8oBobUYR5QSRTIBoANAfdWDoQUwQhF041DSaKBDLZyZKEWRB3AKQClEjlJUuviaZBCIdVqH8WwMBve2d2e3HMIIPxdyRThQkJ4TBdEA==")
	if err != nil {
		t.Fatal(err)
	}
	xz, err := base64.StdEncoding.DecodeString("/Td6WFoAAATm1rRGAgAhARYAAAB0L+Wj4Cf/AG9dADQZSe6N8LrI/5v/8gxprxHnTvz+BUx8eN6h2Fo7PuyuXK+TLRjBXIeER7WKNcYBlPWNmJrFNqLhSwJhmo+GtOL3mCJ7kbSvSIHZYKB+deAiXYT59EeMO+w/SFTlMmEJSO5rZIJgS5EMO/kS5qAAAAAAep8QoTk33XAAAYsBgFAAAP2BpwuxxGf7AgAAAAAEWVo=")
	if err != nil {
		t.Fatal(err)
	}
	zipData := archiveTestBytes(t, "zip", archiveTestEntry{name: "hello.txt", body: "hello world\n", mode: 0640})
	for _, tc := range []struct {
		name, format string
		data         []byte
	}{
		{"zip", "zip", zipData}, {"tar", "tar", plain},
		{"gzip-auto", "tar", gz.Bytes()}, {"bzip2-auto", "tar", bz}, {"xz-auto", "tar", xz},
		{"gzip-explicit", "gzip", gz.Bytes()}, {"bzip2-explicit", "bzip2", bz}, {"xz-explicit", "xz", xz},
		{"tar-flag", "-", plain}, {"gzip-flag", "z", gz.Bytes()}, {"bzip2-flag", "j", bz}, {"xz-flag", "J", xz},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasPrefix(tc.name, "xz") {
				if _, err := exec.LookPath("xz"); err != nil {
					t.Skip("xz decompressor not installed")
				}
			}
			root := t.TempDir()
			dest := filepath.Join(root, "unpacked")
			code, _, stderr := archiveTestRun(t, root, bytes.NewReader(tc.data), "extract", dest, tc.format)
			if code != 0 {
				t.Fatalf("extract exit %d: %s", code, stderr)
			}
			file := filepath.Join(dest, "hello.txt")
			archiveTestRead(t, file, "hello world\n")
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0640 || !info.ModTime().Equal(archiveTestTime) {
				t.Fatalf("metadata: mode %o, time %v", info.Mode().Perm(), info.ModTime())
			}
		})
	}
}

func TestArchiveExtractRelativeSymlinks(t *testing.T) {
	for _, format := range []string{"zip", "tar"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "shared"), []byte("shared data"), 0600); err != nil {
				t.Fatal(err)
			}
			data := archiveTestBytes(t, format,
				archiveTestEntry{name: "folder", kind: tar.TypeDir, mode: 0750},
				archiveTestEntry{name: "folder/data", body: "payload", mode: 0640},
				archiveTestEntry{name: "folder/link", kind: tar.TypeSymlink, link: "data", mode: 0777},
				archiveTestEntry{name: "shared-link", kind: tar.TypeSymlink, link: "../shared", mode: 0777},
			)
			dest := filepath.Join(root, "unpacked")
			code, _, stderr := archiveTestRun(t, root, bytes.NewReader(data), "extract", dest, format)
			if code != 0 {
				t.Fatalf("extract exit %d: %s", code, stderr)
			}
			link := filepath.Join(dest, "folder/link")
			target, err := os.Readlink(link)
			if err != nil || target != "data" {
				t.Fatalf("symlink = %q, %v", target, err)
			}
			archiveTestRead(t, link, "payload")
			archiveTestRead(t, filepath.Join(dest, "shared-link"), "shared data")
			info, err := os.Stat(filepath.Join(dest, "folder"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0750 || !info.ModTime().Equal(archiveTestTime) {
				t.Fatalf("directory metadata: %v", info)
			}
		})
	}
}

func TestArchiveExtractHardlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "shared"), []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}
	data := archiveTestBytes(t, "tar",
		archiveTestEntry{name: "forward-chain", kind: tar.TypeLink, link: "forward"},
		archiveTestEntry{name: "forward", kind: tar.TypeLink, link: "data"},
		archiveTestEntry{name: "data", body: "payload", mode: 0640},
		archiveTestEntry{name: "backward", kind: tar.TypeLink, link: "data"},
		archiveTestEntry{name: "shared-link", kind: tar.TypeLink, link: "../shared"},
	)
	dest := filepath.Join(root, "unpacked")
	code, _, stderr := archiveTestRun(t, root, bytes.NewReader(data), "extract", dest, "tar")
	if code != 0 {
		t.Fatalf("extract exit %d: %s", code, stderr)
	}
	original, err := os.Stat(filepath.Join(dest, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"forward-chain", "forward", "backward"} {
		linked, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(original, linked) {
			t.Fatalf("%s is not a hardlink", name)
		}
	}
	shared, err := os.Stat(filepath.Join(root, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	linked, err := os.Stat(filepath.Join(dest, "shared-link"))
	if err != nil || !os.SameFile(shared, linked) {
		t.Fatalf("root-relative hardlink: %v", err)
	}
}

type archiveSwapReader struct {
	Reader *bytes.Reader
	swap   func()
}

func (r *archiveSwapReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF && r.swap != nil {
		swap := r.swap
		r.swap = nil
		swap()
	}
	return n, err
}

// These tests use the real dispatcher and real archives. Replacing a path when
// the upload finishes models a game process changing it after initial checks.
func TestArchiveExtractOutsideSymlinksAndUploadSwaps(t *testing.T) {
	for _, format := range []string{"zip", "tar"} {
		for _, kind := range []string{"existing-leaf", "existing-parent", "swap-leaf", "swap-parent"} {
			t.Run(format+"/"+kind, func(t *testing.T) {
				root, outside := t.TempDir(), t.TempDir()
				dest := filepath.Join(root, "unpacked")
				if err := os.Mkdir(dest, 0755); err != nil {
					t.Fatal(err)
				}
				victim := filepath.Join(outside, "payload")
				if err := os.WriteFile(victim, []byte("outside sentinel"), 0640); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(victim, archiveTestTime, archiveTestTime); err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(victim)
				if err != nil {
					t.Fatal(err)
				}
				plant := func() {
					if strings.HasSuffix(kind, "parent") {
						if err := os.Rename(dest, dest+"-saved"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(outside, dest); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.Symlink(victim, filepath.Join(dest, "payload")); err != nil {
							t.Fatal(err)
						}
					}
				}
				data := archiveTestBytes(t, format, archiveTestEntry{name: "payload", body: "must not escape", mode: 0777})
				var in io.Reader = bytes.NewReader(data)
				if strings.HasPrefix(kind, "existing") {
					plant()
				} else {
					in = &archiveSwapReader{bytes.NewReader(data), plant}
				}
				code, _, stderr := archiveTestRun(t, root, in, "extract", dest, format)
				if code != 4 {
					t.Fatalf("expected confinement exit 4, got %d: %s", code, stderr)
				}
				archiveTestRead(t, victim, "outside sentinel")
				after, err := os.Stat(victim)
				if err != nil {
					t.Fatal(err)
				}
				if after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
					t.Fatal("outside metadata changed")
				}
			})
		}
	}
}

func TestArchiveRejectsUnsafeEntries(t *testing.T) {
	for _, format := range []string{"zip", "tar"} {
		for _, tc := range []struct {
			name  string
			entry archiveTestEntry
		}{
			{"traversal", archiveTestEntry{name: "../../escaped", body: "bad", mode: 0600}},
			{"absolute", archiveTestEntry{name: "/escaped", body: "bad", mode: 0600}},
			{"symlink-traversal", archiveTestEntry{name: "link", kind: tar.TypeSymlink, link: "../../escaped"}},
			{"symlink-absolute", archiveTestEntry{name: "link", kind: tar.TypeSymlink, link: "/tmp/escaped"}},
			{"fifo", archiveTestEntry{name: "fifo", kind: tar.TypeFifo, mode: 0600}},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				data := archiveTestBytes(t, format, tc.entry)
				code, _, stderr := archiveTestRun(t, root, bytes.NewReader(data), "extract", filepath.Join(root, "unpacked"), format)
				if code != 4 {
					t.Fatalf("expected rejection, got %d: %s", code, stderr)
				}
			})
		}
	}
	for _, tc := range []struct {
		name    string
		entries []archiveTestEntry
	}{
		{"hardlink-traversal", []archiveTestEntry{{name: "link", kind: tar.TypeLink, link: "../../escaped"}}},
		{"hardlink-absolute", []archiveTestEntry{{name: "link", kind: tar.TypeLink, link: "/etc/passwd"}}},
		{"hardlink-missing", []archiveTestEntry{{name: "link", kind: tar.TypeLink, link: "missing"}}},
		{"hardlink-cycle", []archiveTestEntry{{name: "one", kind: tar.TypeLink, link: "two"}, {name: "two", kind: tar.TypeLink, link: "one"}}},
		{"device", []archiveTestEntry{{name: "device", kind: tar.TypeChar, mode: 0600}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			data := archiveTestBytes(t, "tar", tc.entries...)
			code, _, stderr := archiveTestRun(t, root, bytes.NewReader(data), "extract", filepath.Join(root, "unpacked"), "tar")
			if code != 4 {
				t.Fatalf("expected rejection, got %d: %s", code, stderr)
			}
		})
	}
}

func TestArchiveDownloadPreservesLinksWithoutReadingTargets(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "folder")
	if err := os.Mkdir(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("payload"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data", filepath.Join(dir, "relative")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	code, data, stderr := archiveTestRun(t, root, nil, "archive", dir)
	if code != 0 {
		t.Fatalf("archive exit %d: %s", code, stderr)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := make(map[string]*tar.Header)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[header.Name] = header
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("outside secret")) {
			t.Fatal("archive read an outside target")
		}
	}
	for name, target := range map[string]string{"folder/relative": "data", "folder/outside": outside} {
		header := found[name]
		if header == nil || header.Typeflag != tar.TypeSymlink || header.Linkname != target {
			t.Fatalf("link %s: %#v", name, header)
		}
	}
	if len(found) != 4 {
		t.Fatalf("unexpected entries: %v", found)
	}
}

func TestArchiveDecompressRetainsSource(t *testing.T) {
	root := t.TempDir()
	data := archiveTestBytes(t, "zip", archiveTestEntry{name: "data", body: "payload", mode: 0600})
	archive := filepath.Join(root, "source.zip")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := archiveTestRun(t, root, nil, "decompress", archive, "zip")
	if code != 0 {
		t.Fatalf("decompress exit %d: %s", code, stderr)
	}
	archiveTestRead(t, filepath.Join(root, "data"), "payload")
	got, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("source archive changed: %v", err)
	}
}

func TestArchiveExtractionKeepsExistingInternalLinksUseful(t *testing.T) {
	for _, format := range []string{"zip", "tar"} {
		for _, absolute := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/absolute=%v", format, absolute), func(t *testing.T) {
				root := t.TempDir()
				real := filepath.Join(root, "real")
				if err := os.Mkdir(real, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(real, "data"), []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
				dirTarget, fileTarget := "real", "real/data"
				if absolute {
					dirTarget, fileTarget = real, filepath.Join(real, "data")
				}
				if err := os.Symlink(dirTarget, filepath.Join(root, "dirlink")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(fileTarget, filepath.Join(root, "filelink")); err != nil {
					t.Fatal(err)
				}
				data := archiveTestBytes(t, format,
					archiveTestEntry{name: "dirlink/new", body: "new", mode: 0600},
					archiveTestEntry{name: "filelink", body: "updated", mode: 0600})
				code, _, stderr := archiveTestRun(t, root, bytes.NewReader(data), "extract", root, format)
				if code != 0 {
					t.Fatalf("extract exit %d: %s", code, stderr)
				}
				archiveTestRead(t, filepath.Join(real, "new"), "new")
				archiveTestRead(t, filepath.Join(real, "data"), "updated")
			})
		}
	}
}

func TestArchivePreservesAbsoluteInternalLinks(t *testing.T) {
	for _, format := range []string{"tar", "zip"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			if err := os.WriteFile(target, []byte("internal data"), 0600); err != nil {
				t.Fatal(err)
			}
			data := archiveTestBytes(t, format, archiveTestEntry{
				name: "nested/link", kind: tar.TypeSymlink, link: target, mode: 0777,
			})
			code, _, stderr := archiveTestRun(t, root, bytes.NewReader(data), "extract", root, format)
			if code != 0 {
				t.Fatalf("internal symlink rejected: %d %s", code, stderr)
			}
			archiveTestRead(t, filepath.Join(root, "nested/link"), "internal data")
			link, err := os.Readlink(filepath.Join(root, "nested/link"))
			if err != nil || filepath.IsAbs(link) {
				t.Fatalf("link did not become portable: %q %v", link, err)
			}
		})
	}
}
