// Command sftp serves a server's data volume over SFTP (key-only auth), confined
// to the data directory. Like cmd/configrender it has a self-install mode so a
// copy init container drops the static binary onto a shared volume and the
// sidecar runs it from the game image (files owned by the server's user).
//
// Env: QUETZAL_SFTP_ADDR (default :2022), QUETZAL_DATA_PATH (default /data),
// QUETZAL_SFTP_HOST_KEY (PEM file), QUETZAL_SFTP_AUTHORIZED_KEYS (file, re-read
// per connection). Self-install: QUETZAL_INSTALL_TO.
package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"

	"github.com/lolozini/quetzal/internal/sshd"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("sftp: ")

	if dest := os.Getenv("QUETZAL_INSTALL_TO"); dest != "" {
		if err := installSelf(dest); err != nil {
			log.Fatalf("install self: %v", err)
		}
		return
	}

	addr := envOr("QUETZAL_SFTP_ADDR", ":2022")
	root := envOr("QUETZAL_DATA_PATH", "/data")
	hostKeyPath := os.Getenv("QUETZAL_SFTP_HOST_KEY")
	authPath := os.Getenv("QUETZAL_SFTP_AUTHORIZED_KEYS")

	hostKey, err := os.ReadFile(hostKeyPath)
	if err != nil {
		log.Fatalf("read host key %q: %v", hostKeyPath, err)
	}
	srv, err := sshd.New(sshd.Config{
		Addr:    addr,
		Root:    root,
		HostKey: hostKey,
		AuthorizedKeys: func() []ssh.PublicKey {
			return loadAuthorizedKeys(authPath)
		},
	})
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	log.Printf("serving SFTP on %s (root %s)", addr, root)
	if err := srv.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// loadAuthorizedKeys parses an authorized_keys file (best-effort: skips bad
// lines). Returns nil if the file is missing, which denies all access.
func loadAuthorizedKeys(path string) []ssh.PublicKey {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keys []ssh.PublicKey
	rest := data
	for len(rest) > 0 {
		key, _, _, next, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			break
		}
		keys = append(keys, key)
		rest = next
	}
	return keys
}

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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
