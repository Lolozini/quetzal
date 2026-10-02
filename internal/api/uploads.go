package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// Uploads in pieces. A file sent in one request lasts as long as the transfer,
// and the proxies in front of a panel give a request a fixed time to arrive:
// Traefik 3 reads for 60 seconds, then cuts. Sent as pieces of a few seconds
// each, a file of any size gets through any of them, an interrupted upload
// resumes where it stopped, and a piece that failed is the only thing sent
// again.
//
// The pieces gather in a temporary file beside the destination. How much has
// arrived is that file's size, read in the pod, so nothing is kept in memory
// here and nothing is lost if the panel restarts mid-upload.

const (
	// maxUploadSize bounds one upload. The volume is the real limit; this only
	// stops a typing mistake from reserving a session for a petabyte.
	maxUploadSize int64 = 64 << 30
	// maxUploadChunk bounds one piece. The browser sizes its pieces to take a
	// few seconds each, well under this.
	maxUploadChunk int64 = 32 << 20
	// uploadIdle is how long an upload may wait for its next piece before it
	// is collected.
	uploadIdle = 24 * time.Hour
	// uploadChunkTimeout bounds the time one piece may take to arrive.
	uploadChunkTimeout = 15 * time.Minute
	// maxOpenUploads bounds one account's unfinished uploads, each a
	// temporary file on some volume.
	maxOpenUploads = 20
	// fileOpConflict is the scripts' exit code for "not where you think": a
	// piece for the wrong offset, or a finish before the last piece.
	fileOpConflict = 7
)

// tempPath is where an upload's pieces gather: beside the destination file, so
// that finishing is a rename on the same file system; inside the destination
// directory for an archive, as the one-request extract has always done.
func (s *Server) uploadTemp(root string, u *models.FileUpload) string {
	dst := jail(root, u.Path)
	if u.Kind == models.UploadArchive {
		return path.Join(dst, ".quetzal-upload-"+u.ID)
	}
	return dst + ".quetzal-part-" + u.ID
}

// podExec runs a command in a pod; tests replace it to run the scripts here.
func (s *Server) podExec(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod string, cmd []string, stdin io.Reader, stdout io.Writer) error {
	if s.execHook != nil {
		return s.execHook(ctx, cs, cfg, ns, pod, cmd, stdin, stdout)
	}
	return console.Exec(ctx, cs, cfg, ns, pod, cmd, stdin, stdout)
}

// uploadCheckScript refuses an upload that could not be finished, before any
// of it is sent: a file needs its directory and must not name one, and an
// archive's directory is made now, as its pieces gather inside it.
const uploadCheckScript = `qz_guard deref "$0" "$1"
if [ "$2" = archive ]; then
  [ -e "$1" ] && [ ! -d "$1" ] && { echo "not a directory" >&2; exit 4; }
  mkdir -p -- "$1" || exit 1
else
  qz_exists "$(dirname "$1")"
  [ -d "$1" ] && { echo "is a directory" >&2; exit 4; }
fi
:`

// uploadSizeScript prints how many bytes of the upload in $1 have arrived.
const uploadSizeScript = `qz_guard deref "$0" "$1"
if [ -e "$1" ]; then wc -c < "$1" | tr -d ' '; else echo 0; fi`

// uploadAppendScript adds stdin to the upload in $1, provided it holds exactly
// $2 bytes so far, and prints how many it holds after. A piece cut short adds
// only what arrived, which is still the right bytes in the right place: the
// next piece starts from the size printed, and nothing has to be undone.
const uploadAppendScript = drainStdin + `qz_guard deref "$0" "$1"
have=0
[ -e "$1" ] && have=$(wc -c < "$1" | tr -d ' ')
if [ "$have" != "$2" ]; then
  echo "$have"
  echo "the upload holds $have bytes, not $2" >&2
  exit 7
fi
cat >> "$1" || exit 1
wc -c < "$1" | tr -d ' '`

// uploadFinishFileScript moves a complete upload ($1, $3 bytes) into place as
// $2, replacing what was there in one step.
const uploadFinishFileScript = `qz_guard deref "$0" "$1" "$2"
qz_exists "$(dirname "$2")"
[ -d "$2" ] && { echo "is a directory" >&2; exit 4; }
got=0
[ -e "$1" ] && got=$(wc -c < "$1" | tr -d ' ')
[ "$got" = "$3" ] || { echo "the upload holds $got of $3 bytes" >&2; exit 7; }
mv -f -- "$1" "$2"`

// uploadFinishArchiveScript unpacks a complete upload ($1, $3 bytes) into the
// directory $2 with the tool for $4, then removes it.
const uploadFinishArchiveScript = `qz_guard deref "$0" "$1" "$2"
got=0
[ -e "$1" ] && got=$(wc -c < "$1" | tr -d ' ')
[ "$got" = "$3" ] || { echo "the upload holds $got of $3 bytes" >&2; exit 7; }
mkdir -p -- "$2" || exit 1
if [ "$4" = zip ]; then
  unzip -o "$1" -d "$2"; rc=$?
else
  tar -xf "$1" -C "$2"; rc=$?
fi
rm -f -- "$1"
exit $rc`

// uploadRemoveScript deletes an upload's temporary file. The link itself, if
// someone made one there: rm never follows it.
const uploadRemoveScript = `qz_guard link "$0" "$1"
rm -f -- "$1"`

type uploadView struct {
	*models.FileUpload
	// Received is how much has arrived; -1 when it was not looked up.
	Received int64 `json:"received"`
	// ChunkMax is the largest piece the panel takes.
	ChunkMax int64 `json:"chunkMax"`
}

func newUploadID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleCreateUpload starts an upload: where it goes, how big it is, and
// whether it is a file or an archive to unpack.
func (s *Server) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		Kind   string `json:"kind"`
		Format string `json:"format"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	srv, root, cs, cfg, pod, ok := s.fileContext(w, r)
	if !ok || outsideRoot(w, req.Path) {
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = models.UploadFile
	}
	format := ""
	switch kind {
	case models.UploadFile:
		if strings.Trim(path.Clean("/"+req.Path), "/") == "" {
			writeError(w, http.StatusBadRequest, "a file upload needs the name of the file")
			return
		}
	case models.UploadArchive:
		format = "tar"
		if strings.EqualFold(req.Format, "zip") {
			format = "zip"
		}
	default:
		writeError(w, http.StatusBadRequest, `kind must be "file" or "archive"`)
		return
	}
	if req.Size <= 0 || req.Size > maxUploadSize {
		writeError(w, http.StatusBadRequest, "size must be between 1 byte and 64 GiB")
		return
	}
	u := userFrom(r.Context())
	if n, err := s.Store.CountUploadsByUser(u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if n >= maxOpenUploads {
		writeError(w, http.StatusConflict, "too many unfinished uploads; cancel some, or let them expire")
		return
	}
	id, err := newUploadID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id failed")
		return
	}
	up := &models.FileUpload{
		ID: id, ServerID: srv.ID, UserID: u.ID, Path: req.Path, Kind: kind, Format: format,
		Size: req.Size, ExpiresAt: time.Now().Add(uploadIdle),
	}
	ctx, cancel := context.WithTimeout(r.Context(), fileOpTimeout)
	defer cancel()
	if err := s.podExec(ctx, cs, cfg, srv.Namespace, pod,
		[]string{"sh", "-c", guarded(uploadCheckScript), root, jail(root, req.Path), kind}, nil, io.Discard); err != nil {
		writeFileOpError(w, "upload refused", err)
		return
	}
	if err := s.Store.CreateUpload(up); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, uploadView{FileUpload: up, Received: 0, ChunkMax: maxUploadChunk})
}

// handleListUploads lists the caller's unfinished uploads to a server, so that
// choosing the same file again resumes it.
func (s *Server) handleListUploads(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermFiles)
	if !ok {
		return
	}
	ups, err := s.Store.ListUploads(srv.ID, userFrom(r.Context()).ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]uploadView, len(ups))
	for i := range ups {
		out[i] = uploadView{FileUpload: &ups[i], Received: -1, ChunkMax: maxUploadChunk}
	}
	writeJSON(w, http.StatusOK, out)
}

// uploadContext is fileContext for one upload: the server, its pod, and the
// caller's upload named in the path.
func (s *Server) uploadContext(w http.ResponseWriter, r *http.Request) (up *models.FileUpload, srv *models.Server, root string, cs kubernetes.Interface, cfg *rest.Config, pod string, ok bool) {
	srv, root, cs, cfg, pod, ok = s.fileContext(w, r)
	if !ok {
		return
	}
	up, err := s.Store.GetUpload(r.PathValue("uid"), srv.ID, userFrom(r.Context()).ID)
	if err != nil || !up.ExpiresAt.After(time.Now()) {
		writeError(w, http.StatusNotFound, "no such upload; it may have expired")
		return nil, nil, "", nil, nil, "", false
	}
	return up, srv, root, cs, cfg, pod, true
}

// lockUpload claims the upload for this request, answering 409 when another
// request holds it.
func (s *Server) lockUpload(w http.ResponseWriter, up *models.FileUpload, d time.Duration) bool {
	got, err := s.Store.LockUpload(up.ID, time.Now().Add(d))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if !got {
		writeError(w, http.StatusConflict, "another request is working on this upload")
		return false
	}
	return true
}

// handleGetUpload says how much of an upload has arrived.
func (s *Server) handleGetUpload(w http.ResponseWriter, r *http.Request) {
	up, srv, root, cs, cfg, pod, ok := s.uploadContext(w, r)
	if !ok {
		return
	}
	got, err := s.uploadReceived(r.Context(), cs, cfg, srv.Namespace, pod, root, up)
	if err != nil {
		writeFileOpError(w, "could not read the upload", err)
		return
	}
	writeJSON(w, http.StatusOK, uploadView{FileUpload: up, Received: got, ChunkMax: maxUploadChunk})
}

func (s *Server) uploadReceived(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod, root string, up *models.FileUpload) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, fileOpTimeout)
	defer cancel()
	var out strings.Builder
	if err := s.podExec(ctx, cs, cfg, ns, pod,
		[]string{"sh", "-c", guarded(uploadSizeScript), root, s.uploadTemp(root, up)}, nil, &out); err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out.String()), 10, 64)
}

// handlePutUploadChunk adds a piece at ?offset=. The answer says how much has
// arrived since; 409 says where the upload really is when the offset is not.
func (s *Server) handlePutUploadChunk(w http.ResponseWriter, r *http.Request) {
	up, srv, root, cs, cfg, pod, ok := s.uploadContext(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be a byte count")
		return
	}
	n := r.ContentLength
	switch {
	case n < 0:
		writeError(w, http.StatusLengthRequired, "a piece needs its Content-Length")
		return
	case n > maxUploadChunk:
		writeError(w, http.StatusRequestEntityTooLarge, "a piece may hold at most 32 MiB")
		return
	case offset+n > up.Size:
		writeError(w, http.StatusBadRequest, "the piece goes past the size the upload was started with")
		return
	}
	if !s.lockUpload(w, up, uploadChunkTimeout+time.Minute) {
		return
	}
	defer s.Store.TouchUpload(up.ID, time.Now().Add(uploadIdle))

	ctx, cancel := context.WithTimeout(r.Context(), uploadChunkTimeout)
	defer cancel()
	var out strings.Builder
	err = s.podExec(ctx, cs, cfg, srv.Namespace, pod,
		[]string{"sh", "-c", guarded(uploadAppendScript), root, s.uploadTemp(root, up), strconv.FormatInt(offset, 10)},
		http.MaxBytesReader(w, r.Body, n), &out)
	got, perr := strconv.ParseInt(strings.TrimSpace(out.String()), 10, 64)
	var ex *console.ExitError
	switch {
	case errors.As(err, &ex) && ex.Code() == fileOpConflict && perr == nil:
		writeJSON(w, http.StatusConflict, map[string]any{"error": strings.TrimSpace(ex.Stderr), "received": got})
	case err != nil:
		writeFileOpError(w, "the piece could not be written", err)
	case perr != nil:
		writeError(w, http.StatusBadGateway, "the piece was written, but its size could not be read back")
	default:
		writeJSON(w, http.StatusOK, map[string]int64{"received": got})
	}
}

// handleCompleteUpload puts a complete upload in place: the file where it was
// headed, or the archive's contents in its directory.
func (s *Server) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	up, srv, root, cs, cfg, pod, ok := s.uploadContext(w, r)
	if !ok {
		return
	}
	if !s.lockUpload(w, up, fileJobTimeout+time.Minute) {
		return
	}
	tmp, dst, size := s.uploadTemp(root, up), jail(root, up.Path), strconv.FormatInt(up.Size, 10)
	var cmd []string
	timeout := fileOpTimeout
	if up.Kind == models.UploadArchive {
		cmd = []string{"sh", "-c", guarded(uploadFinishArchiveScript), root, tmp, dst, size, up.Format}
		timeout = fileJobTimeout
	} else {
		cmd = []string{"sh", "-c", guarded(uploadFinishFileScript), root, tmp, dst, size}
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	err := s.podExec(ctx, cs, cfg, srv.Namespace, pod, cmd, nil, io.Discard)
	var ex *console.ExitError
	if errors.As(err, &ex) && ex.Code() == fileOpConflict {
		// Not all there yet: the upload stays, to be completed.
		_ = s.Store.TouchUpload(up.ID, time.Now().Add(uploadIdle))
		writeError(w, http.StatusConflict, strings.TrimSpace(ex.Stderr))
		return
	}
	if err != nil && up.Kind == models.UploadFile {
		// Everything arrived; only the last step failed, such as the
		// destination's directory being removed meanwhile. The upload stays,
		// so that finishing can be tried again rather than sending it all
		// over.
		_ = s.Store.TouchUpload(up.ID, time.Now().Add(uploadIdle))
		writeFileOpError(w, "the upload could not be put in place", err)
		return
	}
	// The archive script removes its temporary file whether or not it
	// unpacked, so the upload is over either way.
	_ = s.Store.DeleteUpload(up.ID)
	if err != nil {
		writeFileOpError(w, "extract failed (the image needs tar, or unzip for .zip)", err)
		return
	}
	if up.Kind == models.UploadArchive {
		s.audit(r, srv.ID, "files.extract", up.Path+" ("+up.Format+")")
	} else {
		s.audit(r, srv.ID, "files.write", up.Path)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteUpload cancels an upload and removes what arrived of it.
func (s *Server) handleDeleteUpload(w http.ResponseWriter, r *http.Request) {
	up, srv, root, cs, cfg, pod, ok := s.uploadContext(w, r)
	if !ok {
		return
	}
	if !s.lockUpload(w, up, fileOpTimeout+time.Minute) {
		return
	}
	if err := s.removeUploadTemp(r.Context(), cs, cfg, srv.Namespace, pod, root, up); err != nil {
		_ = s.Store.TouchUpload(up.ID, up.ExpiresAt)
		writeFileOpError(w, "the upload could not be removed", err)
		return
	}
	_ = s.Store.DeleteUpload(up.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeUploadTemp(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod, root string, up *models.FileUpload) error {
	ctx, cancel := context.WithTimeout(ctx, fileOpTimeout)
	defer cancel()
	return s.podExec(ctx, cs, cfg, ns, pod,
		[]string{"sh", "-c", guarded(uploadRemoveScript), root, s.uploadTemp(root, up)}, nil, io.Discard)
}

// uploadGiveUp is how long past its expiry an upload whose temporary file
// cannot be reached is kept before it is forgotten anyway.
const uploadGiveUp = 7 * 24 * time.Hour

// CollectUploads removes the uploads left alone past their expiry, temporary
// file first. A server whose files cannot be reached right now keeps its
// uploads for the next pass, for a week at most.
func (s *Server) CollectUploads(ctx context.Context) {
	ups, err := s.Store.ExpiredUploads()
	if err != nil {
		log.Printf("upload gc: %v", err)
		return
	}
	for i := range ups {
		up := &ups[i]
		srv, err := s.Store.GetServer(up.ServerID)
		if errors.Is(err, store.ErrNotFound) {
			_ = s.Store.DeleteUpload(up.ID)
			continue
		}
		if err == nil {
			err = s.collectUpload(ctx, srv, up)
		}
		switch {
		case err == nil:
			_ = s.Store.DeleteUpload(up.ID)
		case time.Since(up.ExpiresAt) > uploadGiveUp:
			log.Printf("upload gc: giving up on %s of server %d: %v", up.ID, up.ServerID, err)
			_ = s.Store.DeleteUpload(up.ID)
		default:
			log.Printf("upload gc: %s of server %d, next pass: %v", up.ID, up.ServerID, err)
		}
	}
}

func (s *Server) collectUpload(ctx context.Context, srv *models.Server, up *models.FileUpload) error {
	cs, cfg, err := s.clientsFor(srv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pod, err := s.dataPodName(ctx, cs, srv.Namespace, srv.Slug)
	if err != nil {
		return err
	}
	return s.removeUploadTemp(ctx, cs, cfg, srv.Namespace, pod, s.dataRoot(srv), up)
}
