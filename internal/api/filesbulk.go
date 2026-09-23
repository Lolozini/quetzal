package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/console"
)

// File operations beyond the one-path basics: copying, acting on several
// entries at once, and packing or unpacking archives that are already on the
// server. They follow Pterodactyl's file manager, whose users expect them.

// fileJobTimeout bounds the operations that walk or rewrite a lot of data in
// place (copying a world, archiving a modpack, deleting a big folder). They
// answer once done, so the bound has to cover a large server.
const fileJobTimeout = time.Hour

// maxBulkFiles bounds how many entries one request may name.
const maxBulkFiles = 1000

// bulkRequest names entries of one directory.
type bulkRequest struct {
	// Root is the directory, relative to the data root ("" = the root).
	Root  string   `json:"root"`
	Files []string `json:"files"`
}

// checkNames validates entry names: plain names within Root, never a path.
func checkNames(names []string) error {
	if len(names) == 0 {
		return errors.New("no files given")
	}
	if len(names) > maxBulkFiles {
		return fmt.Errorf("at most %d files at once", maxBulkFiles)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
			return fmt.Errorf("invalid file name %q", n)
		}
		if seen[n] {
			return fmt.Errorf("%q is given twice", n)
		}
		seen[n] = true
	}
	return nil
}

// bulkDeleteScript removes every path given after the root.
const bulkDeleteScript = `qz_guard link "$0" "$@"
exec rm -rf -- "$@"`

func (s *Server) handleBulkDelete(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	var req bulkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := checkNames(req.Files); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := jail(root, req.Root)
	cmd := []string{"sh", "-c", guarded(bulkDeleteScript), root}
	for _, f := range req.Files {
		cmd = append(cmd, dir+"/"+f)
	}
	ctx, cancel := context.WithTimeout(r.Context(), fileJobTimeout)
	defer cancel()
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, nil, io.Discard); err != nil {
		writeFileOpError(w, "delete failed", err)
		return
	}
	s.audit(r, srv.ID, "files.delete", describeBulk(req.Root, req.Files))
	w.WriteHeader(http.StatusNoContent)
}

// moveScript moves the entries after $1 into the directory $1, refusing to
// overwrite anything already there.
const moveScript = `dest="$1"; shift
qz_guard deref "$0" "$dest"
[ -d "$dest" ] || { echo "the destination is not a folder" >&2; exit 4; }
qz_guard link "$0" "$@"
for s in "$@"; do qz_exists "$s"; done
for s in "$@"; do
  b=$(basename -- "$s")
  if [ -e "$dest/$b" ] || [ -L "$dest/$b" ]; then
    echo "$b already exists in the destination" >&2; exit 4
  fi
done
exec mv -- "$@" "$dest"`

type moveRequest struct {
	bulkRequest
	// Destination is the folder to move into, relative to the data root.
	Destination string `json:"destination"`
}

func (s *Server) handleBulkMove(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	var req moveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := checkNames(req.Files); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := jail(root, req.Root)
	dest := jail(root, req.Destination)
	cmd := []string{"sh", "-c", guarded(moveScript), root, dest}
	for _, f := range req.Files {
		cmd = append(cmd, dir+"/"+f)
	}
	ctx, cancel := context.WithTimeout(r.Context(), fileJobTimeout)
	defer cancel()
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, nil, io.Discard); err != nil {
		writeFileOpError(w, "move failed", err)
		return
	}
	s.audit(r, srv.ID, "files.move", describeBulk(req.Root, req.Files)+" -> "+req.Destination)
	w.WriteHeader(http.StatusNoContent)
}

// copyScript copies $1 to the first of the following candidate paths that is
// free, and prints the one it used. Links inside a copied folder are copied as
// links (-P), so a copy never pulls in what they point to.
const copyScript = `qz_guard link "$0" "$1"
qz_exists "$1"
src="$1"; shift
for dst in "$@"; do
  if [ ! -e "$dst" ] && [ ! -L "$dst" ]; then
    qz_guard deref "$0" "$dst"
    cp -R -P -p -- "$src" "$dst" || exit 1
    printf '%s' "$dst"
    exit 0
  fi
done
echo "no free name for the copy" >&2; exit 4`

// copyCandidates lists the names a copy may take, as Wings names them:
// "name copy.ext", then "name copy 1.ext" and on. An archive's double extension
// stays whole (world copy.tar.gz), and a dotfile has no extension to split.
func copyCandidates(name string) []string {
	base, ext := splitExt(name)
	out := []string{base + " copy" + ext}
	for i := 1; i <= 50; i++ {
		out = append(out, fmt.Sprintf("%s copy %d%s", base, i, ext))
	}
	return out
}

func splitExt(name string) (string, string) {
	lower := strings.ToLower(name)
	for _, e := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(lower, e) && len(name) > len(e) {
			return name[:len(name)-len(e)], name[len(name)-len(e):]
		}
	}
	ext := path.Ext(name)
	if ext == name {
		return name, ""
	}
	return strings.TrimSuffix(name, ext), ext
}

type copyRequest struct {
	// Path is the file or folder to copy, relative to the data root.
	Path string `json:"path"`
}

func (s *Server) handleCopyFile(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	var req copyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	rel := path.Clean("/" + req.Path)
	if rel == "/" {
		writeError(w, http.StatusBadRequest, "choose a file or folder to copy")
		return
	}
	src := jail(root, rel)
	cmd := []string{"sh", "-c", guarded(copyScript), root, src}
	for _, c := range copyCandidates(path.Base(rel)) {
		cmd = append(cmd, path.Join(path.Dir(src), c))
	}
	ctx, cancel := context.WithTimeout(r.Context(), fileJobTimeout)
	defer cancel()
	var out strings.Builder
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, nil, &out); err != nil {
		writeFileOpError(w, "copy failed", err)
		return
	}
	name := path.Base(out.String())
	s.audit(r, srv.ID, "files.copy", strings.TrimPrefix(rel, "/")+" -> "+name)
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// compressScript packs the entries after $2 (names within the directory $1)
// into the archive $2, written beside them. It is built under a temporary name
// and renamed at the end, so a failure leaves no half archive behind.
const compressScript = `dir="$1"; name="$2"; shift 2
qz_guard deref "$0" "$dir"
qz_exists "$dir"
[ -d "$dir" ] || { echo "not a folder" >&2; exit 4; }
cd "$dir" || exit 1
for f in "$@"; do qz_guard link "$0" "$dir/$f"; qz_exists "$f"; done
if [ -e "$name" ] || [ -L "$name" ]; then echo "$name already exists" >&2; exit 4; fi
tmp=".$name.quetzal-part.$$"
tar -czf "$tmp" -- "$@" || { rm -f "$tmp"; exit 1; }
mv -f "$tmp" "$name"
printf '%s' "$name"`

func (s *Server) handleCompressFiles(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	var req bulkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := checkNames(req.Files); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := "archive-" + time.Now().UTC().Format("2006-01-02-150405") + ".tar.gz"
	cmd := append([]string{"sh", "-c", guarded(compressScript), root, jail(root, req.Root), name}, req.Files...)
	ctx, cancel := context.WithTimeout(r.Context(), fileJobTimeout)
	defer cancel()
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, nil, io.Discard); err != nil {
		writeFileOpError(w, "archive failed (the image needs tar)", err)
		return
	}
	s.audit(r, srv.ID, "files.compress", describeBulk(req.Root, req.Files)+" -> "+name)
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// decompressScript unpacks the archive $1 into the folder that holds it, with
// unzip for a .zip ($2 = zip) and tar otherwise, $2 then being tar's flag for
// the compression (z, j, J, or "-" for none). The flag is given rather than
// left to tar to detect: not every busybox build detects it.
const decompressScript = `qz_guard deref "$0" "$1"
qz_exists "$1"
[ -f "$1" ] || { echo "not a file" >&2; exit 4; }
dir=$(dirname -- "$1")
if [ "$2" = zip ]; then
  command -v unzip >/dev/null 2>&1 || { echo "this server's image has no unzip" >&2; exit 1; }
  exec unzip -o -q "$1" -d "$dir"
fi
[ "$2" = - ] && exec tar -xof "$1" -C "$dir"
exec tar -x"$2"of "$1" -C "$dir"`

// archiveFormat says how an archive is unpacked, from its name: "zip", or
// tar's compression flag ("-" for a plain tar).
func archiveFormat(name string) (string, bool) {
	lower := strings.ToLower(name)
	has := func(exts ...string) bool {
		for _, e := range exts {
			if strings.HasSuffix(lower, e) {
				return true
			}
		}
		return false
	}
	switch {
	case has(".zip"):
		return "zip", true
	case has(".tar"):
		return "-", true
	case has(".tar.gz", ".tgz"):
		return "z", true
	case has(".tar.bz2", ".tbz2"):
		return "j", true
	case has(".tar.xz", ".txz"):
		return "J", true
	}
	return "", false
}

type decompressRequest struct {
	// Path is the archive, relative to the data root.
	Path string `json:"path"`
}

func (s *Server) handleDecompressFile(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	var req decompressRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	format, ok := archiveFormat(req.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "not an archive that can be extracted (.zip, .tar, .tar.gz/.tgz, .tar.bz2, .tar.xz)")
		return
	}
	cmd := []string{"sh", "-c", guarded(decompressScript), root, jail(root, req.Path), format}
	ctx, cancel := context.WithTimeout(r.Context(), fileJobTimeout)
	defer cancel()
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, nil, io.Discard); err != nil {
		writeFileOpError(w, "extract failed", err)
		return
	}
	s.audit(r, srv.ID, "files.decompress", req.Path)
	w.WriteHeader(http.StatusNoContent)
}

// describeBulk summarises a bulk request for the audit log.
func describeBulk(root string, files []string) string {
	shown := files
	if len(shown) > 5 {
		shown = shown[:5]
	}
	d := strings.Join(shown, ", ")
	if len(files) > len(shown) {
		d += fmt.Sprintf(" and %d more", len(files)-len(shown))
	}
	if root = strings.Trim(root, "/"); root != "" {
		d = root + ": " + d
	}
	return d
}
