// Package sftpactivity turns what SFTP sessions change into a server's
// activity. The SFTP sidecar logs each change, one line a change; the
// controller reads that log from where it last stopped and records the changes
// in the audit log and the event feed, as the panel's file manager does its
// own. The sidecar itself holds no credential to the panel and reaches nothing
// in the cluster, which stays so.
package sftpactivity

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// Actions a session's changes are recorded under. setstat is left out: clients
// set a file's times after every upload, which would double each one.
var actions = map[string]string{
	"write":   "sftp.write",
	"mkdir":   "sftp.mkdir",
	"remove":  "sftp.delete",
	"rmdir":   "sftp.delete",
	"rename":  "sftp.rename",
	"symlink": "sftp.symlink",
	"link":    "sftp.link",
}

// Actions is every action this package records, for the event catalogue.
var Actions = []string{"sftp.write", "sftp.mkdir", "sftp.delete", "sftp.rename", "sftp.symlink", "sftp.link"}

// Op is one change a session made.
type Op struct {
	At     time.Time
	User   string
	Action string
	Path   string
}

// Parse reads a sidecar log fetched with timestamps and returns the changes
// made after since, oldest first. Anything else the log holds -- the sidecar
// starting, a line cut short by a size limit -- is skipped.
//
// Times are kept to the microsecond: that is what PostgreSQL stores, and a
// cursor read back rounded down would otherwise find its own line after it.
func Parse(log string, since time.Time) []Op {
	var ops []Op
	for _, line := range strings.Split(log, "\n") {
		op, ok := parseLine(line)
		if ok && op.At.After(since) {
			ops = append(ops, op)
		}
	}
	return ops
}

// parseLine reads `<RFC3339 time> sftp: "<user>" <op> "<path>"`. The user is
// read bare too, as the first sidecars to log changes wrote it.
func parseLine(line string) (Op, bool) {
	ts, rest, ok := strings.Cut(strings.TrimRight(line, "\r"), " ")
	if !ok {
		return Op{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Op{}, false
	}
	rest, ok = strings.CutPrefix(rest, "sftp: ")
	if !ok {
		return Op{}, false
	}
	var user string
	if strings.HasPrefix(rest, `"`) {
		q, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return Op{}, false
		}
		user, _ = strconv.Unquote(q)
		rest = strings.TrimPrefix(rest[len(q):], " ")
	} else if user, rest, ok = strings.Cut(rest, " "); !ok {
		return Op{}, false
	}
	verb, quoted, ok := strings.Cut(rest, " ")
	if !ok || user == "" {
		return Op{}, false
	}
	action, known := actions[verb]
	if !known {
		return Op{}, false
	}
	path, err := strconv.Unquote(quoted)
	if err != nil {
		return Op{}, false
	}
	return Op{At: at.UTC().Truncate(time.Microsecond), User: user, Action: action, Path: path}, true
}

// shownPaths is how many paths one entry names before counting the rest: a
// folder of a thousand files uploaded at once is one line of activity, not a
// thousand.
const shownPaths = 5

// Entry is the changes one person made of one kind, read in one pass.
type Entry struct {
	At     time.Time // the last of them
	User   string
	Action string
	Paths  []string
}

// Detail names the paths, the first few of them when there are many.
func (e Entry) Detail() string {
	if len(e.Paths) <= shownPaths {
		return strings.Join(e.Paths, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(e.Paths[:shownPaths], ", "), len(e.Paths)-shownPaths)
}

// Group gathers ops by person and kind, in the order each first appears.
func Group(ops []Op) []Entry {
	var out []Entry
	at := map[[2]string]int{}
	for _, op := range ops {
		k := [2]string{op.User, op.Action}
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, Entry{User: op.User, Action: op.Action})
		}
		out[i].Paths = append(out[i].Paths, op.Path)
		out[i].At = op.At
	}
	return out
}

// Store is what the collector records into.
type Store interface {
	SFTPCursor(serverID uint) (time.Time, error)
	SetSFTPCursor(serverID uint, seen time.Time) error
	GetUserByUsername(username string) (*models.User, error)
	AddAudit(e *models.AuditEntry) error
	AddEvent(e *models.Event) error
}

// Collector reads the SFTP logs of a cluster's servers.
type Collector struct {
	Store Store
	// Pods names the server's pods running the SFTP sidecar.
	Pods func(ctx context.Context, ns, slug string) ([]string, error)
	// Logs reads the sidecar's log of a pod, with timestamps, from since on
	// (all of it when since is zero).
	Logs func(ctx context.Context, ns, pod string, since time.Time) (string, error)
}

// Collect records what SFTP sessions changed on srv since the last pass.
func (c *Collector) Collect(ctx context.Context, srv *models.Server, ns string) error {
	since, err := c.Store.SFTPCursor(srv.ID)
	if err != nil {
		return err
	}
	pods, err := c.Pods(ctx, ns, srv.Slug)
	if err != nil {
		return err
	}
	var ops []Op
	for _, pod := range pods {
		log, err := c.Logs(ctx, ns, pod, since)
		if err != nil {
			return fmt.Errorf("read the SFTP log of %s: %w", pod, err)
		}
		ops = append(ops, Parse(log, since)...)
	}
	if len(ops) == 0 {
		return nil
	}
	// Two pods only during a replacement, whose logs then interleave.
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].At.Before(ops[j].At) })
	for _, e := range Group(ops) {
		detail := e.Detail()
		a := &models.AuditEntry{CreatedAt: e.At, ServerID: srv.ID, Action: e.Action, Detail: detail, Username: e.User}
		ev := &models.Event{CreatedAt: e.At, ServerID: srv.ID, Type: e.Action, Message: srv.Slug + ": " + detail, Username: e.User}
		if u, err := c.Store.GetUserByUsername(e.User); err == nil {
			a.UserID, ev.UserID = u.ID, u.ID
			a.Username, ev.Username = u.Username, u.Username
		}
		if err := c.Store.AddAudit(a); err != nil {
			return err
		}
		if err := c.Store.AddEvent(ev); err != nil {
			return err
		}
	}
	return c.Store.SetSFTPCursor(srv.ID, ops[len(ops)-1].At)
}
