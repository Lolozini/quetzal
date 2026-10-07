package fileops

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type switchingReader struct {
	switchPath func()
	reader     io.Reader
}

func (r *switchingReader) Read(p []byte) (int, error) {
	if r.switchPath != nil {
		fn := r.switchPath
		r.switchPath = nil
		fn()
	}
	return r.reader.Read(p)
}

func runOperation(t *testing.T, root, op string, in io.Reader, args ...string) (string, int) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Run(append([]string{root, op}, args...), in, &out, &stderr)
	if code != 0 {
		t.Logf("%s: %s", op, stderr.String())
	}
	return out.String(), code
}

func TestAtomicWritePinsParentDuringInput(t *testing.T) {
	for _, parent := range []bool{false, true} {
		name := "leaf"
		if parent {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, "dir")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "file")
			input := &switchingReader{reader: strings.NewReader("payload"), switchPath: func() {
				if parent {
					if err := os.Rename(dir, dir+"-saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, dir); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(outside, dst); err != nil {
					t.Fatal(err)
				}
			}}
			_, code := runOperation(t, root, "write", input, dst, "7")
			if code != 0 {
				t.Fatalf("pinned write failed: %d", code)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside changed: %v %v", entries, err)
			}
			if parent {
				dst = filepath.Join(dir+"-saved", "file")
			}
			b, err := os.ReadFile(dst)
			if err != nil || string(b) != "payload" {
				t.Fatalf("write = %q, %v", b, err)
			}
		})
	}
}

func TestAppendPinsFileDuringInput(t *testing.T) {
	for _, parent := range []bool{false, true} {
		name := "leaf"
		if parent {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, "dir")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "part")
			secret := filepath.Join(outside, "part")
			if err := os.WriteFile(secret, []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			input := &switchingReader{reader: strings.NewReader("payload"), switchPath: func() {
				if parent {
					if err := os.Rename(dir, dir+"-saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, dir); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Rename(dst, dst+"-saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(secret, dst); err != nil {
						t.Fatal(err)
					}
				}
			}}
			out, code := runOperation(t, root, "upload-append", input, dst, "0")
			if code != 0 || out != "7" {
				t.Fatalf("append = %q, %d", out, code)
			}
			b, err := os.ReadFile(secret)
			if err != nil || string(b) != "sentinel" {
				t.Fatalf("outside changed: %q, %v", b, err)
			}
		})
	}
}

func TestHelperRefusesOutsideParentsForEveryOperation(t *testing.T) {
	for _, op := range []string{"list", "read", "archive", "write", "mkdir", "delete", "rename", "move", "copy", "compress", "decompress", "extract", "upload-check", "upload-size", "upload-append", "upload-finish-file", "upload-finish-archive", "upload-remove"} {
		t.Run(op, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			secret := filepath.Join(outside, "secret")
			if err := os.WriteFile(secret, []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(root, "link", "secret")
			args := []string{p}
			switch op {
			case "write", "upload-append":
				args = append(args, "8")
			case "extract", "decompress":
				args = append(args, "zip")
			case "upload-check":
				args = append(args, "file")
			case "rename", "copy", "move":
				args = append(args, filepath.Join(root, "other"))
			case "compress":
				args = append(args, "out.tar.gz", "secret")
			case "upload-finish-file":
				args = append(args, filepath.Join(root, "target"), "8")
			case "upload-finish-archive":
				args = append(args, root, "8", "zip")
			}
			_, code := runOperation(t, root, op, strings.NewReader("payload!"), args...)
			if code != 4 {
				t.Errorf("outside parent returned %d, want 4", code)
			}
			b, err := os.ReadFile(secret)
			if err != nil || string(b) != "sentinel" {
				t.Fatalf("outside changed: %q %v", b, err)
			}
		})
	}
}

func TestHelpersKeepRelativeSymlinksUseful(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "file"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	out, code := runOperation(t, root, "read", nil, filepath.Join(root, "link", "file"))
	if code != 0 || out != "hello" {
		t.Fatalf("in-root link read = %q %d", out, code)
	}
	_, code = runOperation(t, root, "write", strings.NewReader("updated"), filepath.Join(root, "link", "file"), "7")
	if code != 0 {
		t.Fatal(code)
	}
	b, err := os.ReadFile(filepath.Join(root, "real", "file"))
	if err != nil || string(b) != "updated" {
		t.Fatalf("in-root link write = %q %v", b, err)
	}
}
