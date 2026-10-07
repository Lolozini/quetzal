package fileops

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve preserves absolute in-volume symlinks created by older SFTP versions.
// name is root-relative; deref selects whether the leaf itself is followed.
// This normalization is only for compatibility, not confinement: callers MUST
// still perform every operation through the supplied os.Root. Concurrent swaps
// therefore cannot turn the normalized name into an unconfined filesystem call.
func Resolve(root *os.Root, name string, deref bool) (string, error) {
	if name == "" {
		name = "."
	}
	if !filepath.IsLocal(name) {
		return "", bad("path escapes the data directory")
	}
	for range 40 {
		parts := strings.Split(name, string(filepath.Separator))
		replaced := false
		for i := range parts {
			if !deref && i == len(parts)-1 {
				break
			}
			probe := strings.Join(parts[:i+1], string(filepath.Separator))
			fi, err := root.Lstat(probe)
			if os.IsNotExist(err) {
				return name, nil
			}
			if err != nil {
				return "", err
			}
			if fi.Mode()&os.ModeSymlink == 0 {
				continue
			}
			target, err := root.Readlink(probe)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				target, err = filepath.Rel(root.Name(), target)
			} else {
				target = filepath.Join(filepath.Dir(probe), target)
			}
			if err != nil || !filepath.IsLocal(target) {
				return "", bad("path escapes the data directory")
			}
			name = filepath.Join(append([]string{target}, parts[i+1:]...)...)
			replaced = true
			break
		}
		if !replaced {
			return name, nil
		}
	}
	return "", bad("too many symbolic links")
}
