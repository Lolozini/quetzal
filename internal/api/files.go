package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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

// fileStreamTimeout bounds the two operations that stream their output to the
// client instead of buffering it: reading a file and archiving a directory. The
// minute that is right for a mkdir is not right for a 4 GB world: it cuts the
// transfer in the middle, and because the response is already 200 with its
// headers gone, the caller keeps a truncated file and is told nothing.
//
// A client that goes away cancels the request context on its own, so this only
// has to be longer than any download somebody is genuinely still reading. It
// matches the backup Job's deadline for the same reason: it is the point past
// which something is wrong, not a budget anyone should approach.
const fileStreamTimeout = 6 * time.Hour

type fileEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// ModTime is the last modification, in Unix seconds (0 when unknown).
	ModTime int64 `json:"mtime,omitempty"`
	Dir     bool  `json:"dir"`
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
//
// jail confines the path *as text*. A symlink inside the volume still points
// wherever it likes, and nothing stops one appearing there — an extracted
// archive or the game itself can create one. Confinement is therefore enforced
// again inside the container by guardScript, at the moment the path is used.
func jail(root, rel string) string {
	return path.Join(root, path.Clean("/"+rel))
}

// outsideRoot answers 400 when one of paths climbs above the data directory
// ("../x", "a/../../x") and reports whether it did. jail would confine such a
// path, but silently: a write to "../escape.txt" landed in the data directory
// as "escape.txt", and nothing told the caller that the path had been changed.
func outsideRoot(w http.ResponseWriter, paths ...string) bool {
	for _, p := range paths {
		if c := path.Clean(strings.TrimLeft(p, "/")); c == ".." || strings.HasPrefix(c, "../") {
			writeError(w, http.StatusBadRequest, "path escapes the data directory: "+p)
			return true
		}
	}
	return false
}

// guardScript refuses paths that leave the data directory once symlinks are
// taken into account, so a link planted in the volume (by an extracted archive,
// or by the game process) cannot be used to read or write outside it. It runs in
// the container, right before the operation, and is written in plain POSIX shell
// with no external tools: `cd` + `pwd -P` resolves symlinked parent directories
// on every image, where readlink -f is not guaranteed to exist.
//
// Called as: qz_guard <deref|link> <root> <path>...
//   - deref: the operation follows the final component (read, write, list,
//     mkdir, extract), so a symlink there is refused outright.
//   - link:  the operation acts on the link itself and never dereferences it
//     (delete, rename, archive), so a symlink leaf is allowed — otherwise a
//     planted link could never be cleaned up through the panel.
//
// Either way the parent chain must resolve inside the root.
const guardScript = `qz_guard() {
  __mode=$1; __root=$2; shift 2
  __r=$(cd "$__root" 2>/dev/null && pwd -P) || {
    echo "the data directory is not available" >&2; exit 5
  }
  for __p in "$@"; do
    if [ "$__mode" = deref ] && [ -L "$__p" ]; then
      echo "refusing to follow the symbolic link $__p" >&2; exit 4
    fi
    __d=$__p
    # A symlink leaf only gets here in link mode, where the link itself is the
    # subject: probe its parent, since -d and cd would follow it to the target.
    [ -L "$__p" ] && __d=$(dirname "$__p")
    while [ ! -d "$__d" ]; do
      __n=$(dirname "$__d")
      [ "$__n" = "$__d" ] && break
      __d=$__n
    done
    __real=$(cd "$__d" 2>/dev/null && pwd -P) || __real=$__d
    case "$__real" in
      "$__r"|"$__r"/*) ;;
      *) echo "path escapes the data directory" >&2; exit 4 ;;
    esac
  done
}
qz_exists() {
  # -L as well as -e: a dangling symlink still exists as far as the panel is
  # concerned, and has to remain listable, archivable and deletable.
  [ -e "$1" ] || [ -L "$1" ] || {
    echo "no such file or directory" >&2; exit 6
  }
}
`

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
	if q := r.URL.Query(); outsideRoot(w, q.Get("path"), q.Get("to")) {
		return nil, "", nil, nil, "", false
	}
	root = s.dataRoot(srv)
	cs, cfg, err := s.clientsFor(srv)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "target cluster unavailable: "+err.Error())
		return nil, "", nil, nil, "", false
	}
	pod, err = s.dataPodName(r.Context(), cs, srv.Namespace, srv.Slug)
	var unavailable errDataUnavailable
	switch {
	case errors.As(err, &unavailable):
		writeError(w, http.StatusServiceUnavailable, "file management is unavailable: "+unavailable.msg)
		return nil, "", nil, nil, "", false
	case err != nil:
		writeError(w, http.StatusConflict, "file management is temporarily unavailable (the data manager is starting, or a restore is in progress)")
		return nil, "", nil, nil, "", false
	}
	return srv, root, cs, cfg, pod, true
}

// errDataUnavailable is file access refused for a reason waiting will not fix,
// and worth saying. A node that stopped answering used to cost two minutes of
// waiting and end in "the data manager is starting, or a restore is in
// progress".
type errDataUnavailable struct{ msg string }

func (e errDataUnavailable) Error() string { return e.msg }

// unschedulableGrace is how long a data-manager pod may stay unschedulable
// before that counts as an answer: a new server's pod can be refused for a
// moment while its volume is provisioned.
const unschedulableGrace = 20 * time.Second

// dataPodTrouble says why none of pods can serve files when waiting will not
// change it: the node they are on is not responding, or no node can take them.
func dataPodTrouble(ctx context.Context, cs kubernetes.Interface, pods []corev1.Pod) string {
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if c := reconciler.PodCondition(p, corev1.PodScheduled); reconciler.Unschedulable(p) != "" && time.Since(c.LastTransitionTime.Time) > unschedulableGrace {
			return "no node can mount this server's files right now (" + reconciler.Unschedulable(p) + ")"
		}
		if p.Spec.NodeName == "" || !podNotReady(p) {
			continue
		}
		if n, err := cs.CoreV1().Nodes().Get(ctx, p.Spec.NodeName, metav1.GetOptions{}); err == nil && !reconciler.NodeReady(n) {
			return fmt.Sprintf("the node %s, which holds this server's files, is not responding", n.Name)
		}
	}
	return ""
}

// podNotReady reports whether a pod is known not to be ready. The node
// controller marks the pods of a node that stopped answering that way, while
// their containers still read "running", since the kubelet that would say
// otherwise is gone.
func podNotReady(p *corev1.Pod) bool {
	c := reconciler.PodCondition(p, corev1.PodReady)
	return c != nil && c.Status == corev1.ConditionFalse
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
				if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && containerRunning(p, reconciler.WorkloadName) && !podNotReady(p) {
					return p.Name, nil
				}
			}
			if why := dataPodTrouble(ctx, cs, pods.Items); why != "" {
				return "", errDataUnavailable{why}
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

// The guard's exit codes. A path that leaves the data directory is the caller's
// doing -- a mistake, or someone probing -- while an unreachable data directory
// is ours. Both used to come back as 502, which said the data manager was
// broken and made a traversal attempt look like an outage in the logs.
const (
	fileOpBadRequest = 4 // the caller asked for something the jail refuses
	fileOpNoDataRoot = 5 // the data directory itself is unreachable
	fileOpNotFound   = 6 // the path is not there
)

// writeFileOpError answers a failed file operation with the status that matches
// its cause: the caller's when the guard refused the path, ours otherwise.
func writeFileOpError(w http.ResponseWriter, what string, err error) {
	var ex *console.ExitError
	if errors.As(err, &ex) {
		// The script's own words, without the exec plumbing around them.
		switch ex.Code() {
		case fileOpBadRequest:
			writeError(w, http.StatusBadRequest, ex.Stderr)
			return
		case fileOpNotFound:
			writeError(w, http.StatusNotFound, ex.Stderr)
			return
		}
	}
	writeError(w, http.StatusBadGateway, what+": "+err.Error())
}

// countingWriter tracks how many bytes have reached the client, which is what
// decides whether a failure can still be reported or has to abort a response
// that is already in flight.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// streamFailed ends a download that failed.
//
// Before the first byte, that is an ordinary error response. After it, the
// status line said 200 and the headers are long gone: writing a JSON error there
// appends it to the bytes already sent, so the caller stores a truncated file
// carrying "{"error":...}" at the end and a success code in front of it -- which
// is how a half-downloaded world passes for a whole one. There is no way to
// retract a status, so the response is aborted instead. That breaks the
// transfer, which is the one failure every HTTP client already checks for.
func streamFailed(w http.ResponseWriter, what string, sent int64, err error) {
	if sent == 0 {
		// Nothing was written, so the download headers set in advance still
		// describe an attachment that will not be sent.
		w.Header().Del("Content-Disposition")
		writeFileOpError(w, what, err)
		return
	}
	log.Printf("files: %s after %d bytes streamed: %v", what, sent, err)
	panic(http.ErrAbortHandler)
}

// guarded prefixes a file-operation script with the symlink guard and passes the
// data root as $0, so each script keeps its own positional numbering ($1, $2…)
// and simply calls qz_guard on the arguments that are paths.
func guarded(body string) string { return guardScript + body }

// listScript prints "<type>\t<size>\t<mtime>\t<name>" per entry of the
// directory in $1, mtime in Unix seconds (0 where the image has no stat).
// Each record ends with a NUL, the one byte a file name cannot hold: ended by a
// newline, a name with a newline in it (which the game or a plugin can create)
// came out cut in two, its first half listed as a file that was not there and
// the real one out of the panel's reach.
const listScript = `qz_guard deref "$0" "$1"
qz_exists "$1"
cd "$1" 2>/dev/null || { echo "not a directory" >&2; exit 4; }
for e in * .*; do
  [ "$e" = "." ] && continue
  [ "$e" = ".." ] && continue
  [ -e "$e" ] || [ -L "$e" ] || continue
  m=$(stat -c %Y -- "$e" 2>/dev/null) || m=0
  if [ -d "$e" ]; then printf 'd\t0\t%s\t%s\0' "$m" "$e"
  else s=$(wc -c < "$e" 2>/dev/null) || s=0; printf 'f\t%s\t%s\t%s\0' "$s" "$m" "$e"; fi
done`

// parseListing reads listScript's output.
func parseListing(out string) []fileEntry {
	entries := []fileEntry{}
	for _, rec := range strings.Split(out, "\x00") {
		parts := strings.SplitN(rec, "\t", 4)
		if len(parts) != 4 || parts[3] == "" {
			continue
		}
		size, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		mtime, _ := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		entries = append(entries, fileEntry{Name: parts[3], Size: size, ModTime: mtime, Dir: parts[0] == "d"})
	}
	return entries
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	dir := jail(root, r.URL.Query().Get("path"))
	var out strings.Builder
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"sh", "-c", guarded(listScript), root, dir}, nil, &out); err != nil {
		writeFileOpError(w, "list failed", err)
		return
	}
	writeJSON(w, http.StatusOK, parseListing(out.String()))
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
	cmd := []string{"sh", "-c", guarded(`qz_guard deref "$0" "$1"
qz_exists "$1"
[ -d "$1" ] && { echo "is a directory" >&2; exit 4; }
exec cat -- "$1"`), root, full}
	ctx, cancel := context.WithTimeout(r.Context(), fileStreamTimeout)
	defer cancel()
	out := &countingWriter{w: w}
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, nil, out); err != nil {
		streamFailed(w, "read failed", out.n, err)
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
	// Guard the entry being archived ($1/$2), not the directory tar runs from:
	// archiving the whole volume runs from the data root's *parent*, which is
	// legitimately outside the jail. tar stores a symlink as a link rather than
	// following it, so a link leaf is harmless here.
	cmd := []string{"sh", "-c", guarded(`qz_guard link "$0" "$1/$2"
qz_exists "$1/$2"
cd "$1" && exec tar -czf - -- "$2"`), root, parent, base}
	ctx, cancel := context.WithTimeout(r.Context(), fileStreamTimeout)
	defer cancel()
	out := &countingWriter{w: w}
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, nil, out); err != nil {
		streamFailed(w, "archive failed", out.n, err)
	}
}

// extractTimeout bounds an archive upload+extraction (modpacks can be large).
const extractTimeout = 15 * time.Minute

// extractScript unpacks an uploaded archive (read from stdin) into $1, choosing
// the tool from $2 ("zip" or "tar"). It spools to a temp file first because both
// tools need a seekable file to auto-detect the format: tar sniffs gz/bz2/xz
// from the file (it can't from a pipe), and unzip requires a real file.
const extractScript = `qz_guard deref "$0" "$1"
dir="$1"; fmt="$2"
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
	cmd := []string{"sh", "-c", guarded(extractScript), root, dir, format}
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, body, io.Discard); err != nil {
		writeFileOpError(w, "extract failed (the image needs tar, or unzip for .zip)", err)
		return
	}
	s.audit(r, srv.ID, "files.extract", relParam(r)+" ("+format+")")
	w.WriteHeader(http.StatusNoContent)
}

// writeScript writes stdin to $1, atomically and only when the whole payload
// arrived. It spools beside the target and moves the temp file into place at the
// end, so a stream that dies midway (or delivers nothing at all — the exec stdin
// channel can come up empty against a container that has only just started)
// leaves the existing file untouched instead of truncating it to nothing. $2, if
// set, is the byte count expected; a mismatch fails the write.
const writeScript = `qz_guard deref "$0" "$1"
dst="$1"; want="$2"
qz_exists "$(dirname "$dst")"
tmp="$dst.quetzal-part.$$"
cat > "$tmp" || { rm -f "$tmp"; exit 1; }
if [ -n "$want" ]; then
  got=$(wc -c < "$tmp" | tr -d ' ')
  if [ "$got" != "$want" ]; then
    rm -f "$tmp"
    echo "short write: received $got of $want bytes" >&2
    exit 1
  fi
fi
mv -f "$tmp" "$dst"`

// maxRetriableWrite bounds how much of an upload is held in memory so a failed
// write can be retried. Editor saves and config files sit far below it; a larger
// body still streams straight through, but gets a single attempt.
const maxRetriableWrite = 8 << 20 // 8 MiB

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok {
		return
	}
	full := jail(root, r.URL.Query().Get("path"))
	body := http.MaxBytesReader(w, r.Body, 256<<20) // 256 MiB cap

	// Hold a small payload so a lost write can be retried: the first exec into a
	// freshly-started container occasionally delivers no stdin at all, and a
	// second attempt on a warm channel goes through.
	var retry []byte
	var src io.Reader = body
	expected := ""
	if r.ContentLength >= 0 {
		expected = strconv.FormatInt(r.ContentLength, 10)
		if r.ContentLength <= maxRetriableWrite {
			buf, err := io.ReadAll(body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "could not read body")
				return
			}
			retry, src = buf, bytes.NewReader(buf)
		}
	}

	// The body streams through the exec as the client sends it, so the write
	// lasts as long as the upload: the minute a mkdir gets cut every upload that
	// took longer -- a 150 MB modpack jar on a home uplink -- and the file was
	// refused after the whole transfer. The request context still ends it as soon
	// as the client goes away.
	write := func(in io.Reader) error {
		ctx, cancel := context.WithTimeout(r.Context(), fileJobTimeout)
		defer cancel()
		return console.Exec(ctx, cs, cfg, srv.Namespace, pod,
			[]string{"sh", "-c", guarded(writeScript), root, full, expected}, in, io.Discard)
	}
	err := write(src)
	if err != nil && retry != nil {
		err = write(bytes.NewReader(retry))
	}
	if err != nil {
		writeFileOpError(w, "write failed", err)
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
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"sh", "-c", guarded(`qz_guard deref "$0" "$1"
exec mkdir -p -- "$1"`), root, full}, nil, io.Discard); err != nil {
		writeFileOpError(w, "mkdir failed", err)
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
	// A world folder takes longer than a minute to delete on slow storage.
	ctx, cancel := context.WithTimeout(r.Context(), fileJobTimeout)
	defer cancel()
	if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", guarded(`qz_guard link "$0" "$1"
exec rm -rf -- "$1"`), root, full}, nil, io.Discard); err != nil {
		writeFileOpError(w, "delete failed", err)
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
	if err := s.execFile(r.Context(), cs, cfg, srv.Namespace, pod, []string{"sh", "-c", guarded(`qz_guard link "$0" "$1" "$2"
qz_exists "$1"
exec mv -- "$1" "$2"`), root, from, to}, nil, io.Discard); err != nil {
		writeFileOpError(w, "rename failed", err)
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
