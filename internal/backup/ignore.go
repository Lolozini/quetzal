package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// A server lists the files its backups leave out in IgnoreFile, at the root of
// its files, the way a .gitignore does: what Pterodactyl reads from a
// .pteroignore. A Steam game's server is mostly the game itself, gigabytes
// SteamCMD downloads again at will, around a few megabytes of saves.
const (
	IgnoreFile = models.IgnoreFile

	// The limits Wings puts on a .pteroignore: a list written for Pterodactyl
	// is taken or refused the same way here.
	maxIgnoreBytes     = 32 * 1024
	maxIgnorePatterns  = 256
	maxIgnoreWildcards = 16
)

// ResticExcludes turns an ignore list into restic exclude patterns, one per
// line, for the files under root: "/data" for a backup, which reads the volume
// there; "" for a restore, which reads the snapshot's /data as its root.
//
// The list reads as a .gitignore:
//   - a blank line, or one starting with #, holds no pattern; spaces around a
//     pattern are dropped;
//   - a pattern with a / before its end is anchored at the root of the
//     server's files, one without matches at any depth, and a trailing / is
//     dropped (restic has no pattern for directories only);
//   - ! takes back what an earlier pattern left out, though not inside a
//     directory left out whole, as in git;
//   - *, ?, [...], [!...] and ** as in git, \ escaping.
//
// Every pattern stays under root: an unanchored *.sql leaves out the server's
// .sql files, not the database dumps a backup copies beside them.
//
// It refuses what Wings refuses -- a list over 32 KiB, over 256 patterns, a
// pattern with more than 16 wildcards -- and a pattern restic could not read.
func ResticExcludes(list, root string) (string, error) {
	if len(list) > maxIgnoreBytes {
		return "", fmt.Errorf("it is larger than %d KiB", maxIgnoreBytes/1024)
	}
	var out []string
	n := 0
	for i, line := range strings.Split(list, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Trim(line, " ")
		if p == "" {
			continue
		}
		if n++; n > maxIgnorePatterns {
			return "", fmt.Errorf("it has more than %d patterns", maxIgnorePatterns)
		}
		neg := strings.HasPrefix(p, "!")
		if neg {
			p = p[1:]
		}
		if wildcards(p) > maxIgnoreWildcards {
			return "", fmt.Errorf("line %d has more than %d wildcards", i+1, maxIgnoreWildcards)
		}
		p = strings.TrimRight(p, "/")
		anchored := strings.Contains(p, "/")
		p = strings.TrimLeft(p, "/")
		if p == "" {
			continue // "/" names the root itself, which nothing leaves out
		}
		if !anchored {
			p = "**/" + p
		}
		p = root + "/" + bracketNegation(p)
		if err := checkPattern(p); err != nil {
			return "", fmt.Errorf("line %d: %v", i+1, err)
		}
		if neg {
			p = "!" + p
		}
		// restic expands $VAR in an exclude file; $$ is a dollar sign.
		out = append(out, strings.ReplaceAll(p, "$", "$$"))
	}
	return strings.Join(out, "\n"), nil
}

// wildcards counts the unescaped * of a pattern, as Wings does.
func wildcards(p string) int {
	n := 0
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\\':
			i++
		case '*':
			n++
		}
	}
	return n
}

// bracketNegation writes git's [!...] as Go's [^...], which restic matches
// with: Go reads [!...] as a set holding "!".
func bracketNegation(p string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			b.WriteByte(c)
			i++
			b.WriteByte(p[i])
			continue
		case c == '[' && !in:
			in = true
			b.WriteByte(c)
			if i+1 < len(p) && p[i+1] == '!' {
				b.WriteByte('^')
				i++
			}
			continue
		case c == ']' && in:
			in = false
		}
		b.WriteByte(c)
	}
	return b.String()
}

// checkPattern refuses a pattern restic would refuse, failing the backup: a
// malformed [...] or a trailing \.
func checkPattern(p string) error {
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "**" {
			continue
		}
		if _, err := path.Match(part, ""); err != nil {
			return fmt.Errorf("%q is not a valid pattern", part)
		}
	}
	return nil
}

// errNoDataManager is an ignore file that could not be read because no data
// manager is serving the server's files yet: worth waiting for, as the backup
// Job itself waits to land beside one.
var errNoDataManager = errors.New("the server's files are not available")

// ignoreWait bounds how long a backup waits to read the server's ignore file.
// Past it, the backup copies everything, and says why.
const ignoreWait = 2 * time.Minute

// readIgnore reads a server's ignore file through its data manager, as the
// file manager reads a file: "" when it has none.
func readIgnore(ctx context.Context, c cluster.Clients, srv *models.Server, root string) (string, error) {
	if c.Config == nil {
		return "", errNoDataManager
	}
	pods, err := c.Clientset.CoreV1().Pods(srv.Namespace).List(ctx, metav1.ListOptions{LabelSelector: reconciler.DataLabel + "=" + srv.Slug})
	if err != nil {
		return "", errNoDataManager
	}
	pod := ""
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && serving(p) {
			pod = p.Name
			break
		}
	}
	if pod == "" {
		return "", errNoDataManager
	}
	// A link is refused, as the file manager refuses to read one: it could
	// point anywhere, a file of another server's included.
	script := `cd "$1" 2>/dev/null || exit 5
[ -L "$2" ] && { echo "it is a symbolic link" >&2; exit 4; }
[ -f "$2" ] || exit 0
exec cat -- "$2"`
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out := &cappedBuffer{max: maxIgnoreBytes}
	err = console.Exec(ctx, c.Clientset, c.Config, srv.Namespace, pod, []string{"sh", "-c", script, "sh", root, IgnoreFile}, nil, out)
	if out.over {
		return "", fmt.Errorf("it is larger than %d KiB", maxIgnoreBytes/1024)
	}
	var exit *console.ExitError
	switch {
	case errors.As(err, &exit) && exit.Code() == 4:
		return "", errors.New(exit.Stderr)
	case errors.As(err, &exit) && exit.Code() == 5:
		return "", errNoDataManager
	case err != nil:
		return "", fmt.Errorf("it could not be read: %v", err)
	}
	return out.String(), nil
}

// serving reports whether a data-manager pod's container runs and is ready.
func serving(p *corev1.Pod) bool {
	for _, c := range p.Status.ContainerStatuses {
		if c.Name == reconciler.WorkloadName {
			return c.State.Running != nil && c.Ready
		}
	}
	return false
}

// cappedBuffer keeps what is written to it up to max bytes, and refuses more,
// which ends the read.
type cappedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.over = true
		return 0, errors.New("ignore file too large")
	}
	return b.Buffer.Write(p)
}
