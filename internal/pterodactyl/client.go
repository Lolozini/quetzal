// Package pterodactyl is a small client for the Pterodactyl (and Pelican) panel
// *client* API, used to import a server into Quetzal: its settings (egg,
// variables, limits, allocations) and its data (a backup archive).
//
// It uses the client API rather than the application API on purpose: a client
// key (ptlc_…) is something every panel user can create for the servers they
// own or share, where an application key needs the panel's administrator. The
// price is that the client API does not expose the egg's install script — the
// egg itself has to exist in Quetzal already, imported from its JSON.
package pterodactyl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one panel with one API key.
type Client struct {
	Base string // panel root, e.g. https://panel.example.com (no trailing slash)
	Key  string
	HTTP *http.Client
}

// ParseServerURL splits what a user pastes into the panel root and the server
// identifier. The natural thing to paste is the address bar of the server's
// page (https://panel.example.com/server/1a2b3c4d/files); a bare panel URL plus
// a separate identifier is accepted too.
func ParseServerURL(raw, identifier string) (base, id string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", errors.New("enter the panel address, e.g. https://panel.example.com/server/1a2b3c4d")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", errors.New("the panel address must start with https://")
	}
	id = strings.TrimSpace(identifier)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	prefix := ""
	for i, p := range parts {
		if p == "server" && i+1 < len(parts) {
			if id == "" {
				id = parts[i+1]
			}
			break
		}
		// A panel served under a sub-path keeps that path as its root.
		if p != "" {
			prefix += "/" + p
		}
	}
	if id == "" {
		return "", "", errors.New("could not find the server in that address: paste the address of the server's page (…/server/<id>)")
	}
	if !validIdentifier(id) {
		return "", "", errors.New("the server identifier is not valid")
	}
	return u.Scheme + "://" + u.Host + prefix, id, nil
}

// validIdentifier accepts the panel's short identifier or a full UUID; it keeps
// anything that could alter the request path out of it.
func validIdentifier(id string) bool {
	if len(id) < 4 || len(id) > 36 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Error is a non-2xx answer from the panel.
type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "the panel refused the API key: use a client API key (ptlc_…), created under Account → API Credentials"
	case http.StatusForbidden:
		if e.Detail != "" {
			return "the panel refused the request: " + e.Detail
		}
		return "the API key does not have access to this server"
	case http.StatusNotFound:
		return "the panel has no such server for this API key"
	case http.StatusTooManyRequests:
		return "the panel is rate-limiting requests; try again in a few minutes"
	}
	if e.Detail != "" {
		return fmt.Sprintf("the panel answered %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("the panel answered %d", e.Status)
}

// maxJSON bounds any JSON answer from the panel.
const maxJSON = 4 << 20

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/vnd.pterodactyl.v1+json")
	req.Header.Set("User-Agent", "quetzal-import")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the panel: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON))
	if err != nil {
		return fmt.Errorf("reading the panel's answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{Status: resp.StatusCode, Detail: errorDetail(data)}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("the panel's answer is not the Pterodactyl API (is this the panel address?)")
	}
	return nil
}

// errorDetail extracts the first message of a Pterodactyl error body
// ({"errors":[{"detail":"…"}]}).
func errorDetail(data []byte) string {
	var e struct {
		Errors []struct {
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &e) == nil && len(e.Errors) > 0 {
		d := e.Errors[0].Detail
		if len(d) > 300 {
			d = d[:300]
		}
		return d
	}
	return ""
}

// Server is what the import needs from a panel server.
type Server struct {
	Identifier  string
	Name        string
	DockerImage string
	Invocation  string
	EggName     string
	EggFeatures []string
	MemoryMB    int64
	CPUPercent  int64
	DiskMB      int64
	BackupLimit int
	Allocations []Allocation
	Variables   []Variable
	Installing  bool
	Suspended   bool
}

// Allocation is one of the server's ports.
type Allocation struct {
	Port    int32
	Default bool
}

// Variable is a startup variable the key can see (hidden ones are not listed by
// the client API).
type Variable struct {
	Env      string
	Value    string // server_value, or the default when the server has none
	Editable bool
}

type envelope[T any] struct {
	Attributes T `json:"attributes"`
}

type list[T any] struct {
	Data []envelope[T] `json:"data"`
}

// GetServer loads the server with its variables, allocations and egg.
func (c *Client) GetServer(ctx context.Context, id string) (*Server, error) {
	var raw envelope[struct {
		Identifier    string   `json:"identifier"`
		Name          string   `json:"name"`
		DockerImage   string   `json:"docker_image"`
		Invocation    string   `json:"invocation"`
		EggFeatures   []string `json:"egg_features"`
		IsSuspended   bool     `json:"is_suspended"`
		IsInstalling  bool     `json:"is_installing"`
		Status        *string  `json:"status"`
		Limits        struct{ Memory, CPU, Disk int64 }
		FeatureLimits struct {
			Backups int `json:"backups"`
		} `json:"feature_limits"`
		Relationships struct {
			Allocations list[struct {
				Port      int32 `json:"port"`
				IsDefault bool  `json:"is_default"`
			}] `json:"allocations"`
			Variables list[struct {
				Env        string  `json:"env_variable"`
				Default    string  `json:"default_value"`
				Value      *string `json:"server_value"`
				IsEditable bool    `json:"is_editable"`
			}] `json:"variables"`
			Egg *envelope[struct {
				Name string `json:"name"`
			}] `json:"egg"`
		} `json:"relationships"`
	}]
	if err := c.do(ctx, http.MethodGet, "/api/client/servers/"+id+"?include=egg,allocations,variables", nil, &raw); err != nil {
		return nil, err
	}
	a := raw.Attributes
	if a.Identifier == "" && a.Name == "" {
		return nil, errors.New("the panel's answer is not a server (is this the panel address?)")
	}
	s := &Server{
		Identifier:  a.Identifier,
		Name:        a.Name,
		DockerImage: a.DockerImage,
		Invocation:  a.Invocation,
		EggFeatures: a.EggFeatures,
		MemoryMB:    a.Limits.Memory,
		CPUPercent:  a.Limits.CPU,
		DiskMB:      a.Limits.Disk,
		BackupLimit: a.FeatureLimits.Backups,
		Installing:  a.IsInstalling || (a.Status != nil && *a.Status == "installing"),
		Suspended:   a.IsSuspended,
	}
	if e := a.Relationships.Egg; e != nil {
		s.EggName = e.Attributes.Name
	}
	for _, al := range a.Relationships.Allocations.Data {
		s.Allocations = append(s.Allocations, Allocation{Port: al.Attributes.Port, Default: al.Attributes.IsDefault})
	}
	for _, v := range a.Relationships.Variables.Data {
		val := v.Attributes.Default
		if v.Attributes.Value != nil {
			val = *v.Attributes.Value
		}
		s.Variables = append(s.Variables, Variable{Env: v.Attributes.Env, Value: val, Editable: v.Attributes.IsEditable})
	}
	return s, nil
}

// DiskUsage returns the bytes the server currently uses on disk (0 when the
// panel cannot tell, e.g. the node is offline).
func (c *Client) DiskUsage(ctx context.Context, id string) int64 {
	var raw envelope[struct {
		Resources struct {
			DiskBytes int64 `json:"disk_bytes"`
		} `json:"resources"`
	}]
	if c.do(ctx, http.MethodGet, "/api/client/servers/"+id+"/resources", nil, &raw) != nil {
		return 0
	}
	return raw.Attributes.Resources.DiskBytes
}

// Backup is a panel backup's state.
type Backup struct {
	UUID       string  `json:"uuid"`
	Successful bool    `json:"is_successful"`
	Bytes      int64   `json:"bytes"`
	Completed  *string `json:"completed_at"`
}

// CreateBackup starts a backup of the whole server.
func (c *Client) CreateBackup(ctx context.Context, id, name string) (*Backup, error) {
	var raw envelope[Backup]
	if err := c.do(ctx, http.MethodPost, "/api/client/servers/"+id+"/backups", map[string]any{"name": name}, &raw); err != nil {
		return nil, err
	}
	if raw.Attributes.UUID == "" {
		return nil, errors.New("the panel did not return the backup it created")
	}
	return &raw.Attributes, nil
}

// GetBackup reads a backup's state.
func (c *Client) GetBackup(ctx context.Context, id, uuid string) (*Backup, error) {
	var raw envelope[Backup]
	if err := c.do(ctx, http.MethodGet, "/api/client/servers/"+id+"/backups/"+url.PathEscape(uuid), nil, &raw); err != nil {
		return nil, err
	}
	return &raw.Attributes, nil
}

// WaitBackup polls a backup until it completes. tick is called on every poll so
// the caller can report progress (and keep its heartbeat fresh).
func (c *Client) WaitBackup(ctx context.Context, id, uuid string, every time.Duration, tick func()) (*Backup, error) {
	for {
		b, err := c.GetBackup(ctx, id, uuid)
		if err != nil {
			return nil, err
		}
		if b.Completed != nil {
			if !b.Successful {
				return nil, errors.New("the panel's backup failed (see the panel for why)")
			}
			return b, nil
		}
		if tick != nil {
			tick()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

// BackupDownloadURL returns the signed URL serving a backup's archive.
func (c *Client) BackupDownloadURL(ctx context.Context, id, uuid string) (string, error) {
	return c.signedURL(ctx, "/api/client/servers/"+id+"/backups/"+url.PathEscape(uuid)+"/download")
}

// DeleteBackup removes a backup.
func (c *Client) DeleteBackup(ctx context.Context, id, uuid string) error {
	return c.do(ctx, http.MethodDelete, "/api/client/servers/"+id+"/backups/"+url.PathEscape(uuid), nil, nil)
}

// ListRoot returns the names of the entries at the root of the server.
func (c *Client) ListRoot(ctx context.Context, id string) ([]string, error) {
	var raw list[struct {
		Name string `json:"name"`
	}]
	if err := c.do(ctx, http.MethodGet, "/api/client/servers/"+id+"/files/list?directory=%2F", nil, &raw); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raw.Data))
	for _, e := range raw.Data {
		names = append(names, e.Attributes.Name)
	}
	return names, nil
}

// Compress archives the given root entries into a .tar.gz at the server root
// and returns the archive's name.
func (c *Client) Compress(ctx context.Context, id string, files []string) (string, error) {
	var raw envelope[struct {
		Name string `json:"name"`
	}]
	if err := c.do(ctx, http.MethodPost, "/api/client/servers/"+id+"/files/compress",
		map[string]any{"root": "/", "files": files}, &raw); err != nil {
		return "", err
	}
	if raw.Attributes.Name == "" {
		return "", errors.New("the panel did not return the archive it created")
	}
	return raw.Attributes.Name, nil
}

// FileDownloadURL returns the signed URL serving a file of the server.
func (c *Client) FileDownloadURL(ctx context.Context, id, file string) (string, error) {
	return c.signedURL(ctx, "/api/client/servers/"+id+"/files/download?file="+url.QueryEscape("/"+file))
}

// DeleteRootFiles deletes entries at the server root.
func (c *Client) DeleteRootFiles(ctx context.Context, id string, files []string) error {
	return c.do(ctx, http.MethodPost, "/api/client/servers/"+id+"/files/delete",
		map[string]any{"root": "/", "files": files}, nil)
}

func (c *Client) signedURL(ctx context.Context, path string) (string, error) {
	var raw envelope[struct {
		URL string `json:"url"`
	}]
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return "", err
	}
	u, err := url.Parse(raw.Attributes.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("the panel returned an unusable download link")
	}
	return raw.Attributes.URL, nil
}

// Open streams a signed download. The link carries its own token, so no
// credentials are sent with it (it usually points at the node, not the panel).
func (c *Client) Open(ctx context.Context, signed string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "quetzal-import")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("could not reach the node serving the archive: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, 0, &Error{Status: resp.StatusCode, Detail: errorDetail(data)}
	}
	return resp.Body, resp.ContentLength, nil
}
