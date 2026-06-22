// Command configrender renders a server's egg config.files into its data volume
// at startup. It runs as an init container in two steps so file ownership is
// always correct: a "copy" step (the Quetzal image) installs this static binary
// into a shared volume, then a "render" step (the game image, i.e. the same user
// as the server) executes it against the data volume.
//
// Inputs (env): QUETZAL_CONFIG_FILES (JSON array of {path,parser,find}),
// QUETZAL_DATA_PATH (default /data). In copy mode, QUETZAL_INSTALL_TO names the
// destination path to copy this binary to.
package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/lolozini/quetzal/internal/configfile"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("configrender: ")

	if dest := os.Getenv("QUETZAL_INSTALL_TO"); dest != "" {
		if err := installSelf(dest); err != nil {
			log.Fatalf("install self: %v", err)
		}
		return
	}

	raw := strings.TrimSpace(os.Getenv("QUETZAL_CONFIG_FILES"))
	if raw == "" {
		return // nothing to render
	}
	var specs []configfile.Spec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		log.Fatalf("parse QUETZAL_CONFIG_FILES: %v", err)
	}
	root := os.Getenv("QUETZAL_DATA_PATH")
	if root == "" {
		root = "/data"
	}
	// Best-effort: a single bad file must not crash-loop the server.
	if err := configfile.Render(root, specs, os.Getenv); err != nil {
		log.Printf("rendering config files (continuing): %v", err)
	}
}

// installSelf copies this executable to dest (world-readable/executable) so the
// next init container, running as the game's user, can execute it.
func installSelf(dest string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(0o755)
}
