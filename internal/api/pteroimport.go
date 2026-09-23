package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/egg"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/pterodactyl"
	"github.com/lolozini/quetzal/internal/safefetch"
)

// Importing a server from Pterodactyl happens in two steps that the web UI
// chains into one gesture:
//
//  1. inspect: read the source server through the panel's client API and turn it
//     into a draft of the create form (template, image, limits, ports, variables),
//     with warnings for whatever does not carry over;
//  2. create with a "pterodactyl" source: the server is created stopped, then a
//     background job has the panel produce an archive of the server (a backup, or
//     a compressed copy when the server has no backup slot), streams it into the
//     new volume and marks the server installed, so the egg's install script does
//     not run over the imported files.
//
// The API key lives only in memory for the duration of the job and is never
// stored; a failed import is retried by supplying it again.

// pteroSource is where to import from.
type pteroSource struct {
	// URL is the address of the server's page on the panel
	// (https://panel.example.com/server/1a2b3c4d), or the panel root together
	// with Identifier.
	URL        string `json:"url"`
	Identifier string `json:"identifier,omitempty"`
	APIKey     string `json:"apiKey"`
}

// pteroClient builds a panel client for src. Outbound traffic goes through the
// SSRF-guarded transport, like every other fetch of a user-supplied URL.
func (s *Server) pteroClient(src pteroSource) (*pterodactyl.Client, string, error) {
	base, id, err := pterodactyl.ParseServerURL(src.URL, src.Identifier)
	if err != nil {
		return nil, "", err
	}
	key := strings.TrimSpace(src.APIKey)
	if key == "" {
		return nil, "", errors.New("an API key is required")
	}
	if strings.HasPrefix(key, "ptla_") {
		return nil, "", errors.New("that is an application API key (ptla_…): use a client API key (ptlc_…), created under Account → API Credentials on the panel")
	}
	hc := s.PteroHTTP
	if hc == nil {
		t := safefetch.SafeTransport()
		// The panel's compress endpoint answers only once the archive is
		// written, which takes minutes on a large server; every call carries
		// its own deadline instead.
		t.ResponseHeaderTimeout = 0
		hc = &http.Client{Transport: t, CheckRedirect: safefetch.CheckRedirect}
	}
	return &pterodactyl.Client{Base: base, Key: key, HTTP: hc}, id, nil
}

// pteroDraft is the create form, pre-filled from the source server.
type pteroDraft struct {
	Name     string `json:"name"`
	Template string `json:"template,omitempty"`
	Image    string `json:"image,omitempty"`
	Memory   string `json:"memory,omitempty"`
	CPU      string `json:"cpu,omitempty"`
	Storage  string `json:"storage"`
	// Ports are editor rows ("TCP/UDP" = both, which is what a Pterodactyl
	// allocation is); the first one is the primary.
	Ports []pteroPortRow `json:"ports"`
	// Env holds the source's values for the matched template's editable
	// variables. Variables maps every variable the key could see, so the form
	// can re-map them if the user picks another template.
	Env       map[string]string `json:"env"`
	Variables map[string]string `json:"variables"`
}

type pteroPortRow struct {
	Port     string `json:"port"`
	Protocol string `json:"protocol"`
}

type pteroInspectResult struct {
	Source struct {
		Name          string `json:"name"`
		Identifier    string `json:"identifier"`
		Egg           string `json:"egg"`
		DockerImage   string `json:"dockerImage"`
		DiskUsedBytes int64  `json:"diskUsedBytes"`
		BackupLimit   int    `json:"backupLimit"`
	} `json:"source"`
	Draft    pteroDraft `json:"draft"`
	Warnings []string   `json:"warnings"`
}

// handleInspectPterodactyl reads a server from a Pterodactyl panel and returns
// the create form it maps to. Nothing is created.
func (s *Server) handleInspectPterodactyl(w http.ResponseWriter, r *http.Request) {
	var src pteroSource
	if err := decodeJSON(r, &src); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	c, id, err := s.pteroClient(src)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ps, err := c.GetServer(ctx, id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	templates, err := s.Store.ListTemplates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := buildPteroDraft(ps, c.DiskUsage(ctx, id), templates)
	writeJSON(w, http.StatusOK, res)
}

// buildPteroDraft maps a panel server onto Quetzal's create form.
func buildPteroDraft(ps *pterodactyl.Server, diskUsed int64, templates []models.Template) pteroInspectResult {
	var res pteroInspectResult
	res.Source.Name = ps.Name
	res.Source.Identifier = ps.Identifier
	res.Source.Egg = ps.EggName
	res.Source.DockerImage = ps.DockerImage
	res.Source.DiskUsedBytes = diskUsed
	res.Source.BackupLimit = ps.BackupLimit
	warn := func(f string, a ...any) { res.Warnings = append(res.Warnings, fmt.Sprintf(f, a...)) }

	d := &res.Draft
	d.Name = ps.Name
	if ps.MemoryMB > 0 {
		d.Memory = strconv.FormatInt(ps.MemoryMB, 10) + "Mi"
	}
	if ps.CPUPercent > 0 {
		d.CPU = strconv.FormatInt(ps.CPUPercent*10, 10) + "m"
	}
	d.Storage = volumeSizeFor(ps.DiskMB, diskUsed)
	d.Variables = map[string]string{}
	for _, v := range ps.Variables {
		d.Variables[v.Env] = v.Value
	}
	d.Env = map[string]string{}

	allocs := append([]pterodactyl.Allocation(nil), ps.Allocations...)
	sort.SliceStable(allocs, func(i, j int) bool { return allocs[i].Default && !allocs[j].Default })
	for _, a := range allocs {
		d.Ports = append(d.Ports, pteroPortRow{Port: strconv.Itoa(int(a.Port)), Protocol: "TCP/UDP"})
	}

	if ps.Installing {
		warn("the server is still installing on the panel: its files may be incomplete")
	}
	if ps.Suspended {
		warn("the server is suspended on the panel: the panel may refuse to archive it")
	}

	tmpl := matchTemplate(templates, ps.EggName)
	if tmpl == nil {
		warn("no template matches the egg %q: import that egg first (Admin → Templates), or pick the template to use", ps.EggName)
		return res
	}
	d.Template = tmpl.Slug
	if len(tmpl.Ports) > 0 {
		// The template fixes its ports; the source's allocations do not apply.
		d.Ports = nil
	}
	if ps.DockerImage != "" {
		if templateOffersImage(tmpl, ps.DockerImage) {
			d.Image = ps.DockerImage
		} else {
			warn("the image %s is not one of the template's; the template's default is used", ps.DockerImage)
		}
	}
	seen := map[string]bool{}
	for _, v := range tmpl.Variables {
		seen[v.EnvVariable] = true
		val, ok := d.Variables[v.EnvVariable]
		if !ok {
			continue
		}
		if !v.Editable {
			if val != v.Default {
				warn("%s is fixed by the template (%q here, %q on the panel)", v.EnvVariable, v.Default, val)
			}
			continue
		}
		if v.Type == models.VarEnum && len(v.Options) > 0 && !containsString(v.Options, val) {
			warn("%s=%q is not one of the template's choices; its default is used", v.EnvVariable, val)
			continue
		}
		d.Env[v.EnvVariable] = val
	}
	var unknown []string
	for k := range d.Variables {
		if !seen[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		warn("the template has no variable %s: those values are not carried over", strings.Join(unknown, ", "))
	}
	return res
}

// matchTemplate finds the template imported from the named egg: by name, then
// by the slug the egg import would have given it.
func matchTemplate(templates []models.Template, eggName string) *models.Template {
	eggName = strings.TrimSpace(eggName)
	if eggName == "" {
		return nil
	}
	for i := range templates {
		if strings.EqualFold(strings.TrimSpace(templates[i].Name), eggName) {
			return &templates[i]
		}
	}
	slug := egg.Slugify(eggName)
	for i := range templates {
		if templates[i].Slug == slug {
			return &templates[i]
		}
	}
	return nil
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// volumeSizeFor sizes the new volume: the source's disk limit when it has one,
// and never less than what the data takes today plus room to grow.
func volumeSizeFor(limitMB, usedBytes int64) string {
	const gi = 1 << 30
	need := int64(math.Ceil(float64(usedBytes)*1.5/gi)) + 1
	size := int64(10)
	if limitMB > 0 {
		size = int64(math.Ceil(float64(limitMB) / 1024))
	}
	if size < need {
		size = need
	}
	if size < 1 {
		size = 1
	}
	return strconv.FormatInt(size, 10) + "Gi"
}

// importInProgress answers 409 while a server's data is being imported: starting
// it would run the egg's install over the arriving files, and a restore or a
// transfer would race the import for the volume.
func importInProgress(w http.ResponseWriter, srv *models.Server) bool {
	if srv.Import.Running(time.Now()) {
		writeError(w, http.StatusConflict, "the server's data is still being imported")
		return true
	}
	return false
}

// importJobs keeps one import per server at a time within this process.
var importJobs sync.Map // server ID -> struct{}

// startPteroImport records the import and runs it in the background.
func (s *Server) startPteroImport(srv *models.Server, c *pterodactyl.Client, id string, startAfter bool) (*models.ImportState, error) {
	if _, busy := importJobs.LoadOrStore(srv.ID, struct{}{}); busy {
		return nil, errors.New("an import is already running for this server")
	}
	now := time.Now()
	host := c.Base
	if u, err := url.Parse(c.Base); err == nil {
		host = u.Host
	}
	st := &models.ImportState{
		Phase:      models.ImportPreparing,
		Source:     host + " / " + id,
		Message:    "asking the panel for an archive of the server",
		StartedAt:  now,
		UpdatedAt:  now,
		StartAfter: startAfter,
	}
	if err := s.Store.SetServerImport(srv.ID, st); err != nil {
		importJobs.Delete(srv.ID)
		return nil, err
	}
	snapshot := *st
	go func() {
		defer importJobs.Delete(srv.ID)
		s.runPteroImport(srv, c, id, st)
	}()
	return &snapshot, nil
}

// pteroImportTimeout bounds a whole import. It is long because the archive is a
// whole game server, and the panel may take a while to produce it.
const pteroImportTimeout = 6 * time.Hour

// importHeartbeat is how often a running import rewrites its state, which both
// reports progress and proves the job is alive (see ImportState.Running).
var importHeartbeat = 10 * time.Second

// pteroPollEvery is how often the panel's backup is polled.
var pteroPollEvery = 5 * time.Second

func (s *Server) runPteroImport(srv *models.Server, c *pterodactyl.Client, id string, st *models.ImportState) {
	ctx, cancel := context.WithTimeout(context.Background(), pteroImportTimeout)
	defer cancel()

	var mu sync.Mutex
	var bytes atomic.Int64
	save := func(mutate func(*models.ImportState)) {
		mu.Lock()
		defer mu.Unlock()
		if mutate != nil {
			mutate(st)
		}
		st.Bytes = bytes.Load()
		st.UpdatedAt = time.Now()
		_ = s.Store.SetServerImport(srv.ID, st)
	}
	stopBeat := make(chan struct{})
	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		t := time.NewTicker(importHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-t.C:
				save(nil)
			}
		}
	}()

	cleanup, err := s.importData(ctx, srv, c, id, &bytes, save)
	close(stopBeat)
	<-beatDone
	if cleanup != nil {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanup(cctx)
		ccancel()
	}
	if err != nil {
		save(func(st *models.ImportState) {
			st.Phase = models.ImportFailed
			st.Message = err.Error()
		})
		return
	}
	save(func(st *models.ImportState) {
		st.Phase = models.ImportDone
		st.Message = ""
	})
	if st.StartAfter {
		_ = s.Store.StartServer(srv.ID, time.Now())
	}
}

// importData produces the archive on the panel and streams it into the volume.
// The returned cleanup removes what was created on the panel for the import.
func (s *Server) importData(ctx context.Context, srv *models.Server, c *pterodactyl.Client, id string, n *atomic.Int64, save func(func(*models.ImportState))) (func(context.Context), error) {
	signed, cleanup, err := pteroArchive(ctx, c, id, save)
	if err != nil {
		return cleanup, err
	}
	body, total, err := c.Open(ctx, signed)
	if err != nil {
		return cleanup, err
	}
	defer body.Close()
	save(func(st *models.ImportState) {
		st.Phase = models.ImportDownloading
		st.Message = "copying the files into the volume"
		if total > 0 {
			st.Total = total
		}
	})
	sink := s.ImportSink
	if sink == nil {
		sink = s.extractIntoVolume
	}
	if err := sink(ctx, srv, &progressReader{r: body, n: n}); err != nil {
		return cleanup, fmt.Errorf("unpacking the archive failed: %w", err)
	}
	return cleanup, nil
}

// pteroArchive has the panel produce an archive of the whole server and returns
// a link to download it. A backup is the natural choice (a consistent snapshot,
// and it honours the server's .pteroignore); a server without a free backup
// slot — hosts often give none — gets a compressed copy of its root instead.
func pteroArchive(ctx context.Context, c *pterodactyl.Client, id string, save func(func(*models.ImportState))) (string, func(context.Context), error) {
	b, err := c.CreateBackup(ctx, id, "quetzal-import")
	if err == nil {
		cleanup := func(ctx context.Context) { _ = c.DeleteBackup(ctx, id, b.UUID) }
		save(func(st *models.ImportState) { st.Message = "the panel is backing up the server" })
		if _, err := c.WaitBackup(ctx, id, b.UUID, pteroPollEvery, nil); err != nil {
			return "", cleanup, err
		}
		signed, err := c.BackupDownloadURL(ctx, id, b.UUID)
		return signed, cleanup, err
	}
	var pe *pterodactyl.Error
	if !errors.As(err, &pe) || pe.Status == http.StatusUnauthorized || pe.Status == http.StatusNotFound {
		return "", nil, err
	}
	// No backup possible (disabled, limit reached, no permission): compress.
	save(func(st *models.ImportState) {
		st.Message = "the server has no free backup slot; the panel is compressing its files instead"
	})
	names, lerr := c.ListRoot(ctx, id)
	if lerr != nil {
		return "", nil, lerr
	}
	if len(names) == 0 {
		return "", nil, errors.New("the server has no files on the panel")
	}
	archive, cerr := c.Compress(ctx, id, names)
	if cerr != nil {
		return "", nil, fmt.Errorf("could not back up (%v) nor compress the server: %w", err, cerr)
	}
	cleanup := func(ctx context.Context) { _ = c.DeleteRootFiles(ctx, id, []string{archive}) }
	signed, err := c.FileDownloadURL(ctx, id, archive)
	return signed, cleanup, err
}

// progressReader counts the bytes that went through it.
type progressReader struct {
	r io.Reader
	n *atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	k, err := p.r.Read(b)
	p.n.Add(int64(k))
	return k, err
}

// ImportSink writes an imported archive (a gzipped tar) into a server's volume.
type ImportSink func(ctx context.Context, srv *models.Server, archive io.Reader) error

// importScript unpacks a gzipped tar from stdin into the data root ($0), then
// writes the install marker with the server's generation ($1) so the egg's
// install does not run over the imported files. Ownership recorded in the
// archive is not restored (-o): the files belong to the user the data manager
// runs as, which is the server's.
const importScript = `qz_guard deref "$0" "$0"
cd "$0" || exit 1
tar -xzof - || exit 1
printf '%s' "$1" > .quetzal-installed`

// importPodWait bounds the wait for a new server's data manager: its volume is
// provisioned and its image pulled first.
const importPodWait = 15 * time.Minute

// extractIntoVolume is the default ImportSink: it streams the archive into tar
// in the server's data-manager pod.
func (s *Server) extractIntoVolume(ctx context.Context, srv *models.Server, archive io.Reader) error {
	cs, cfg, err := s.clientsFor(srv)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(importPodWait)
	var pod string
	for {
		pod, err = s.dataPodName(ctx, cs, srv.Namespace, srv.Slug)
		if err == nil {
			break
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return errors.New("the server's volume did not become available")
		}
	}
	gen := srv.InstallGeneration
	if cur, err := s.Store.GetServer(srv.ID); err == nil {
		gen = cur.InstallGeneration
	}
	cmd := []string{"sh", "-c", guarded(importScript), s.dataRoot(srv), strconv.Itoa(gen)}
	return console.Exec(ctx, cs, cfg, srv.Namespace, pod, cmd, archive, io.Discard)
}

type pteroImportRequest struct {
	pteroSource
	// Start starts the server once its data is in place.
	Start bool `json:"start"`
}

// handleImportPterodactyl imports a Pterodactyl server's data into an existing,
// stopped server (and is how a failed import is retried). Files already in the
// volume are overwritten by the archive's, others are kept.
func (s *Server) handleImportPterodactyl(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermFiles)
	if !ok {
		return
	}
	// It rewrites the server's files and its install state: both the files and
	// the settings permission.
	if !s.can(userFrom(r.Context()), srv, models.PermSettings) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req pteroImportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if transferInProgress(w, srv) || importInProgress(w, srv) {
		return
	}
	if srv.DesiredState != models.StateStopped || srv.Status.Phase != models.PhaseStopped {
		writeError(w, http.StatusConflict, "stop the server first: the import writes into its volume")
		return
	}
	c, id, err := s.pteroClient(req.pteroSource)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	st, err := s.startPteroImport(srv, c, id, req.Start)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.audit(r, srv.ID, "server.import", st.Source)
	writeJSON(w, http.StatusAccepted, st)
}
