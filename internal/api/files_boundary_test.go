package api

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteConfinementDuringUpload(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(fmt.Sprint("parent=", parent), func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, "folder")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "file")
			cmd := fileopTestCommand(root, "write", dst, "7")
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				in.Close()
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			// Opening the spool is the synchronization point: validation finished,
			// but the upload cannot commit until this test closes its input.
			deadline := time.After(5 * time.Second)
			var spool string
			for {
				entries, _ := os.ReadDir(dir)
				if len(entries) != 0 {
					spool = entries[0].Name()
					break
				}
				select {
				case <-deadline:
					t.Fatal("upload never opened its spool")
				case <-time.After(time.Millisecond):
				}
			}
			if parent {
				if err := os.WriteFile(filepath.Join(outside, spool), []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(dir, dir+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(outside, dst); err != nil {
				t.Fatal(err)
			}
			if _, err := in.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			in.Close()
			_ = cmd.Wait()
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if parent {
				b, err := os.ReadFile(filepath.Join(outside, spool))
				if len(entries) != 1 || err != nil || string(b) != "outside" {
					t.Fatalf("upload altered outside parent: %v, %q, %v (%s)", entries, b, err, output.String())
				}
			} else if len(entries) != 0 {
				t.Fatalf("upload escaped root: %v (%s)", entries, output.String())
			}
		})
	}
}

func TestZipExistingSymlinkConfinement(t *testing.T) {
	for _, op := range []string{"extract", "decompress", "finish"} {
		t.Run(op, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			archive := makeZip(t, "link/probe.txt", "outside")
			stored := filepath.Join(root, "archive.zip")
			if err := os.WriteFile(stored, archive, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{root, "zip"}
			operation := "extract"
			switch op {
			case "decompress":
				operation, args = "decompress", []string{stored, "zip"}
			case "finish":
				operation, args = "upload-finish-archive", []string{stored, root, fmt.Sprint(len(archive)), "zip"}
			}
			cmd := fileopTestCommand(root, operation, args...)
			cmd.Stdin = bytes.NewReader(archive)
			out, _ := cmd.CombinedOutput()
			if b, err := os.ReadFile(filepath.Join(outside, "probe.txt")); !os.IsNotExist(err) {
				t.Fatalf("ZIP escaped root: %q, %v (%s)", b, err, out)
			}
		})
	}
}
