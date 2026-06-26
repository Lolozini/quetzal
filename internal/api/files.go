package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// File operations run inside the server's running container via the exec
// subresource (no sidecar). Paths from the client are always confined to the
// server's data directory and passed as positional shell arguments (never
// interpolated), so neither path traversal nor shell injection is possible.

const fileOpTimeout = 60 * time.Second

type fileEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

// dataRoot returns the server's data directory (the only writable, mounted path).
func (s *Server) dataRoot(srv *models.Server) string {
	if t, err := s.Store.GetTemplate(srv.TemplateID); err == nil && t.DataPath != "" {
		return t.DataPath
	}
	return "/data"
}

// jail resolves a client-supplied relative path strictly under root. Any "..":
// path.Clean against "/" first drops leading parent refs, so the join can never
// escape root.
func jail(root, rel string) string {
	return path.Join(root, path.Clean("/"+rel))
}

// defaultDataReadyTimeout bounds how long file access waits for the data-manager
// pod to become running (it may need scheduling and an image pull). Overridable
// via Server.DataReadyTimeout (e.g. in tests).
const defaultDataReadyTimeout = 2 * time.Minute

// fileContext loads the server (requiring the files permission), resolves its
// data root and target cluster, and finds the data-manager pod to operate in.
// The data-manager mounts the data volume permanently, so file operations work
// whether the game is running or stopped. It writes the error response itself and
// returns ok=false on any failure.
func (s *Server) fileContext(w http.ResponseWriter, r *http.Request) (srv *models.Server, root string, cs kubernetes.Interface, cfg *rest.Config, pod string, ok bool) {
	srv, ok = s.requireServer(w, r, models.PermFiles)
	if !ok {
		return
	}
	root = s.dataRoot(srv)
	cs, cfg, err := s.clientsFor(srv)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "target cluster unavailable: "+err.Error())
		return nil, "", nil, nil, "", false
	}
	// Suspension is an admin-enforced freeze: owners and subusers lose file
	// access just like power (matching Pterodactyl). Admins may still inspect the
	// files (e.g. to investigate why it was suspended).
	if srv.DesiredState == models.StateSuspended {
		if u := userFrom(r.Context()); u == nil || !u.HasAdminPerm(models.AdminPermServers) {
			writeError(w, http.StatusForbidden, "server is suspended")
			return nil, "", nil, nil, "", false
		}
	}
	pod, err = s.dataPodName(r.Context(), cs, srv.Namespace, srv.Slug)
	if err != nil {
		writeError(w, http.StatusConflict, "file management is temporarily unavailable (the data manager is starting, or a restore is in progress)")
		return nil, "", nil, nil, "", false
	}
	return srv, root, cs, cfg, pod, true
}

// dataPodName returns the name of the server's running data-manager pod, waiting
// briefly if it is still starting. The data-manager is the permanent pod (created
// by the reconciler) that mounts the data volume; it is scaled to zero only
// during a restore, in which case this returns an error.
func (s *Server) dataPodName(ctx context.Context, cs kubernetes.Interface, ns, slug string) (string, error) {
	timeout := s.DataReadyTimeout
	if timeout <= 0 {
		timeout = defaultDataReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: reconciler.DataLabel + "=" + slug})
		if err == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && containerRunning(p, reconciler.WorkloadName) {
					return p.Name, nil
				}
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return "", fmt.Errorf("data manager pod not ready")
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// containerRunning reports whether the named container in the pod is running.
func containerRunning(p *corev1.Pod, name string) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == name {
			return cs.State.Running != nil
		}
	}
	return false
}

// exec runs a command in the server's container with a bounded timeout.
func (s *Server) execFile(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod string, cmd []string, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, fileOpTimeout)
	defer cancel()
	return console.Exec(ctx, cs, cfg, ns, pod, cmd, stdin, stdout)
}

// listScript prints "<type>\t<size>\t<name>" per entry of the directory in $1.
const listScript = `cd "$1" 2>/dev/null || { echo "no such directory" >&2; exit 2; }
for e in * .*; do
  [ "$e" = "." ] && continue
  [ "$e" = ".." ] && continue
  [ -e "$e" ] || [ -L "$e" ] || continue
  if [ -d "$e" ]; then printf 'd\t0\t%s\n' "$e"
  else s=$(wc -c < "$e" 2>/dev/null) || s=0; printf 'f\t%s\t%s\n' "$s" "$e"; fi
done`

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	dir := jail(root, r.URL.Query().Get("path"))
	var out strings.Builder
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"sh", "-c", listScript, "_", dir}, nil, &out); err != nil {
		writeError(w, http.StatusBadGateway, "list failed: "+err.Error())
		return
	}
	entries := []fileEntry{}
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		size, _ := strconv.ParseInt(parts[1], 10, 64)
		entries = append(entries, fileEntry{Name: parts[2], Size: size, Dir: parts[0] == "d"})
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	full := jail(root, r.URL.Query().Get("path"))
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(path.Base(full))+`"`)
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	// Stream cat's stdout straight to the response (no buffering of large files).
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"cat", "--", full}, nil, w); err != nil {
		// Headers may already be sent; best-effort error only if nothing written.
		writeError(w, http.StatusBadGateway, "read failed: "+err.Error())
		return
	}
}

// handleArchiveFile streams a gzip tarball of a file or directory, so whole
// folders can be downloaded (not just single files).
func (s *Server) handleArchiveFile(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	full := jail(root, r.URL.Query().Get("path"))
	parent, base := path.Dir(full), path.Base(full)
	name := base
	if name == "/" || name == "." || name == "" {
		name = "files"
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(name)+`.tar.gz"`)
	// tar from the parent so the archive contains the entry by its bare name.
	cmd := []string{"sh", "-c", `cd "$1" && exec tar -czf - -- "$2"`, "_", parent, base}
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, cmd, nil, w); err != nil {
		writeError(w, http.StatusBadGateway, "archive failed: "+err.Error())
	}
}

// extractTimeout bounds an archive upload+extraction (modpacks can be large).
const extractTimeout = 15 * time.Minute

// extractScript unpacks an uploaded archive (read from stdin) into $1, choosing
// the tool from $2 ("zip" or "tar"). It spools to a temp file first because both
// tools need a seekable file to auto-detect the format: tar sniffs gz/bz2/xz
// from the file (it can't from a pipe), and unzip requires a real file.
const extractScript = `dir="$1"; fmt="$2"
mkdir -p "$dir" || exit 1
tmp="$dir/.quetzal-upload.$$"
cat > "$tmp" || { rm -f "$tmp"; exit 1; }
if [ "$fmt" = zip ]; then
  unzip -o "$tmp" -d "$dir"; rc=$?
else
  tar -xf "$tmp" -C "$dir"; rc=$?
fi
rm -f "$tmp"
exit $rc`

// handleExtractArchive uploads an archive and unpacks it into a directory of the
// server's data volume — for importing a world, a modpack, or a Pterodactyl
// backup. The archive streams through the exec into tar/unzip in the pod.
func (s *Server) handleExtractArchive(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	dir := jail(root, r.URL.Query().Get("path"))
	format := "tar"
	if strings.EqualFold(r.URL.Query().Get("format"), "zip") {
		format = "zip"
	}
	body := http.MaxBytesReader(w, r.Body, 2<<30) // 2 GiB cap
	ctx, cancel := context.WithTimeout(r.Context(), extractTimeout)
	defer cancel()
	cmd := []string{"sh", "-c", extractScript, "_", dir, format}
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, body, io.Discard); err != nil {
		writeError(w, http.StatusBadGateway, "extract failed (the image needs tar, or unzip for .zip): "+err.Error())
		return
	}
	s.audit(r, srv.ID, "files.extract", relParam(r)+" ("+format+")")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	full := jail(root, r.URL.Query().Get("path"))
	body := http.MaxBytesReader(w, r.Body, 256<<20) // 256 MiB cap
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"sh", "-c", `cat > "$1"`, "_", full}, body, io.Discard); err != nil {
		writeError(w, http.StatusBadGateway, "write failed: "+err.Error())
		return
	}
	s.audit(r, srv.ID, "files.write", relParam(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	full := jail(root, r.URL.Query().Get("path"))
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"mkdir", "-p", "--", full}, nil, io.Discard); err != nil {
		writeError(w, http.StatusBadGateway, "mkdir failed: "+err.Error())
		return
	}
	s.audit(r, srv.ID, "files.mkdir", relParam(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	if path.Clean("/"+rel) == "/" {
		writeError(w, http.StatusBadRequest, "refusing to delete the data root")
		return
	}
	full := jail(root, rel)
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"rm", "-rf", "--", full}, nil, io.Discard); err != nil {
		writeError(w, http.StatusBadGateway, "delete failed: "+err.Error())
		return
	}
	s.audit(r, srv.ID, "files.delete", rel)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRenameFile(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	from := jail(root, r.URL.Query().Get("path"))
	toRel := r.URL.Query().Get("to")
	if strings.TrimSpace(toRel) == "" {
		writeError(w, http.StatusBadRequest, "missing 'to'")
		return
	}
	to := jail(root, toRel)
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"mv", "--", from, to}, nil, io.Discard); err != nil {
		writeError(w, http.StatusBadGateway, "rename failed: "+err.Error())
		return
	}
	s.audit(r, srv.ID, "files.rename", relParam(r)+" -> "+toRel)
	w.WriteHeader(http.StatusNoContent)
}

func relParam(r *http.Request) string { return r.URL.Query().Get("path") }

// sanitizeFilename strips characters unsafe for a Content-Disposition filename.
func sanitizeFilename(name string) string {
	return strings.NewReplacer(`"`, "", "\\", "", "\n", "", "\r", "").Replace(name)
}
