package fileops

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// writeArchive writes a gzip-compressed tar without dereferencing symbolic links.
// names are root-relative source paths; bases are their names inside the archive.
func writeArchive(root *os.Root, names []string, bases []string, out io.Writer) (err error) {
	if len(names) != len(bases) {
		return bad("archive source/name count mismatch")
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	// Do not emit a gzip trailer/header after an early failure: before any data,
	// the HTTP caller can still return the original 400/404 instead of a broken
	// successful download. Mid-stream failures deliberately leave it truncated.
	defer func() {
		if err == nil {
			err = errors.Join(tw.Close(), gz.Close())
		}
	}()
	for i, name := range names {
		base, e := archiveName(bases[i])
		if e != nil {
			return e
		}
		if e = writeArchiveEntry(root, name, base, tw); e != nil {
			return e
		}
	}
	return nil
}

func writeArchiveEntry(root *os.Root, name, base string, tw *tar.Writer) (err error) {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(name)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, target)
		if err != nil {
			return err
		}
		header.Name = base
		return tw.WriteHeader(header)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return bad("unsupported archive source %q", name)
	}
	// A checked leaf can be swapped before opening: do not follow it, and do not
	// block on a FIFO. Use the opened descriptor's type and metadata thereafter.
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err = file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return bad("unsupported archive source %q", name)
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = base
	if info.IsDir() {
		header.Name += "/"
	}
	if err = tw.WriteHeader(header); err != nil {
		return err
	}
	if !info.IsDir() {
		_, err = io.CopyN(tw, file, info.Size())
		return err
	}
	for {
		entries, readErr := file.ReadDir(128)
		for _, entry := range entries {
			if err = writeArchiveEntry(root, filepath.Join(name, entry.Name()), path.Join(base, entry.Name()), tw); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// archiveName rejects traversal rather than cleaning it into an unrelated name.
func archiveName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || path.IsAbs(name) {
		return "", bad("invalid archive path %q", name)
	}
	name = path.Clean(name)
	if name == ".." || strings.HasPrefix(name, "../") {
		return "", bad("archive path escapes destination: %q", name)
	}
	return name, nil
}

type archiveDirectory struct {
	name string
	mode os.FileMode
	time time.Time
}

type archiveHardlink struct {
	name   string
	target string
}

type archiveExtractor struct {
	root        *os.Root
	dir         string
	directories []archiveDirectory
	links       []archiveHardlink
}

// extractArchive never closes or removes file: the caller owns the pinned spool.
// "tar" auto-detects uncompressed, gzip, bzip2, and xz tar streams.
func extractArchive(root *os.Root, dir string, file *os.File, format string) (err error) {
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err = root.MkdirAll(dir, 0755); err != nil {
		return err
	}
	x := archiveExtractor{root: root, dir: dir}
	if strings.EqualFold(format, "zip") {
		err = x.extractZIP(file)
	} else {
		switch format {
		case "-":
			format = "tar"
		case "z":
			format = "gzip"
		case "j":
			format = "bzip2"
		case "J":
			format = "xz"
		}
		err = x.extractTar(file, strings.ToLower(format))
	}
	if err != nil {
		return err
	}
	return x.finish()
}

func (x *archiveExtractor) destination(name string) (string, error) {
	name, err := archiveName(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(x.dir, filepath.FromSlash(name)), nil
}

func (x *archiveExtractor) directory(name string, mode os.FileMode, modified time.Time) error {
	var err error
	name, err = Resolve(x.root, name, true)
	if err != nil {
		return err
	}
	if err := x.root.MkdirAll(name, 0755); err != nil {
		return err
	}
	// Apply directory permissions after children, opening a fresh confined fd
	// then rather than holding a descriptor for every directory in a large tree.
	x.directories = append(x.directories, archiveDirectory{name, mode.Perm(), modified})
	return nil
}

func archiveMetadata(file *os.File, mode os.FileMode, modified time.Time) error {
	if err := file.Chmod(mode.Perm()); err != nil {
		return err
	}
	return fileTimes(file, modified)
}

// fileTimes changes timestamps on the pinned descriptor, never on a path that
// could have been replaced by a symbolic link after opening.
func fileTimes(file *os.File, modified time.Time) error {
	if modified.IsZero() {
		return nil
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return err
	}
	stamp := syscall.NsecToTimeval(modified.UnixNano())
	var timeErr error
	if err = raw.Control(func(fd uintptr) {
		timeErr = syscall.Futimes(int(fd), []syscall.Timeval{stamp, stamp})
	}); err != nil {
		return err
	}
	return timeErr
}

func (x *archiveExtractor) regular(name string, mode os.FileMode, modified time.Time, in io.Reader) (err error) {
	name, err = Resolve(x.root, name, true)
	if err != nil {
		return err
	}
	if err = x.root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	file, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if errors.Is(err, syscall.ELOOP) {
		return bad("refusing archive destination symlink %q", name)
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return bad("archive destination is not a regular file: %q", name)
	}
	if err = file.Truncate(0); err != nil {
		return err
	}
	if _, err = io.Copy(file, in); err != nil {
		return err
	}
	return archiveMetadata(file, mode, modified)
}

func archiveLinkTarget(base, target string) (string, error) {
	if target == "" || filepath.IsAbs(target) || strings.ContainsRune(target, '\x00') {
		return "", bad("invalid archive link target %q", target)
	}
	resolved := filepath.Join(base, target)
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return "", bad("archive link escapes root: %q", target)
	}
	return resolved, nil
}

func (x *archiveExtractor) symlink(name, target string) error {
	// Downloads can contain old absolute links that still point inside this
	// data volume. Store them relatively so they remain root-confined; an
	// absolute target outside the volume is still refused.
	if filepath.IsAbs(target) {
		relativeTarget, err := relative(x.root, target)
		if err != nil {
			return err
		}
		target, err = filepath.Rel(filepath.Dir(name), relativeTarget)
		if err != nil {
			return err
		}
	}
	resolved, err := archiveLinkTarget(filepath.Dir(name), target)
	if err != nil {
		return err
	}
	if _, err := x.root.Stat(resolved); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := x.prepareLink(name); err != nil {
		return err
	}
	return x.root.Symlink(target, name)
}

func (x *archiveExtractor) prepareLink(name string) error {
	if err := x.root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	// Remove only the leaf, never recursively remove a directory on overwrite.
	if err := x.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (x *archiveExtractor) extractZIP(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(file, info.Size())
	if err != nil {
		return fmt.Errorf("invalid zip archive: %w", err)
	}
	for _, entry := range zr.File {
		name, err := x.destination(entry.Name)
		if err != nil {
			return err
		}
		mode := entry.Mode()
		if mode.IsDir() {
			if err = x.directory(name, mode, entry.Modified); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
			return bad("unsupported zip entry %q", entry.Name)
		}
		in, err := entry.Open()
		if err != nil {
			return err
		}
		if mode&os.ModeSymlink != 0 {
			var target []byte
			target, err = io.ReadAll(io.LimitReader(in, 4097))
			if err == nil && len(target) > 4096 {
				err = bad("archive symlink target too long")
			}
			if err == nil {
				err = x.symlink(name, string(target))
			}
		} else {
			err = x.regular(name, mode, entry.Modified, in)
		}
		err = errors.Join(err, in.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func (x *archiveExtractor) extractTar(file *os.File, format string) (err error) {
	br := bufio.NewReader(file)
	magic, peekErr := br.Peek(6)
	if peekErr != nil && !errors.Is(peekErr, io.EOF) {
		return peekErr
	}
	var in io.Reader = br
	if format == "tar" {
		switch {
		case len(magic) >= 2 && string(magic[:2]) == "\x1f\x8b":
			format = "gzip"
		case len(magic) >= 3 && string(magic[:3]) == "BZh":
			format = "bzip2"
		case string(magic) == "\xfd7zXZ\x00":
			format = "xz"
		}
	}
	switch format {
	case "tar":
	case "gzip", "gz", "tar.gz", "tgz":
		gz, e := gzip.NewReader(br)
		if e != nil {
			return fmt.Errorf("invalid gzip archive: %w", e)
		}
		defer func() { err = errors.Join(err, gz.Close()) }()
		in = gz
	case "bzip2", "bz2", "tar.bz2", "tbz2":
		in = bzip2.NewReader(br)
	case "xz", "tar.xz", "txz":
		// xz only decompresses a byte stream. It never receives a path and
		// cannot choose any destination; all archive filesystem work is here.
		cmd := exec.Command("xz", "-dc")
		cmd.Stdin = br
		pipe, e := cmd.StdoutPipe()
		if e != nil {
			return e
		}
		if e = cmd.Start(); e != nil {
			return errors.Join(e, pipe.Close())
		}
		defer func() {
			if err != nil {
				_ = cmd.Process.Kill()
			}
			closeErr := pipe.Close()
			waitErr := cmd.Wait()
			err = errors.Join(err, closeErr, waitErr)
		}()
		in = pipe
	default:
		return bad("unsupported archive format %q", format)
	}
	tr := tar.NewReader(in)
	for {
		header, e := tr.Next()
		if errors.Is(e, io.EOF) {
			// Read compression trailers as well, surfacing checksum/truncation
			// errors and allowing the xz process to finish before Wait.
			_, err = io.Copy(io.Discard, in)
			return err
		}
		if e != nil {
			return fmt.Errorf("invalid tar archive: %w", e)
		}
		name, e := x.destination(header.Name)
		if e != nil {
			return e
		}
		switch header.Typeflag {
		case tar.TypeDir:
			err = x.directory(name, header.FileInfo().Mode(), header.ModTime)
		case tar.TypeReg, tar.TypeRegA:
			err = x.regular(name, header.FileInfo().Mode(), header.ModTime, tr)
		case tar.TypeSymlink:
			err = x.symlink(name, header.Linkname)
		case tar.TypeLink:
			var target string
			target, err = archiveLinkTarget(x.dir, header.Linkname)
			if err == nil {
				x.links = append(x.links, archiveHardlink{name, target})
			}
		default:
			err = bad("unsupported tar entry type %d for %q", header.Typeflag, header.Name)
		}
		if err != nil {
			return err
		}
	}
}

func (x *archiveExtractor) finish() error {
	for len(x.links) != 0 {
		pending := x.links[:0]
		progress := false
		for _, link := range x.links {
			info, err := x.root.Lstat(link.target)
			if errors.Is(err, os.ErrNotExist) {
				pending = append(pending, link)
				continue
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return bad("archive hardlink target is not a regular file: %q", link.target)
			}
			if link.name == link.target {
				return bad("archive hardlink points to itself: %q", link.name)
			}
			if err = x.prepareLink(link.name); err != nil {
				return err
			}
			// Root.Link does not follow the source leaf, even if it is swapped
			// for a symlink after Lstat. Subsequent accesses remain root-bound.
			if err = x.root.Link(link.target, link.name); err != nil {
				return err
			}
			progress = true
		}
		if !progress {
			return bad("archive contains missing or cyclic hardlink targets")
		}
		x.links = pending
	}
	for i := len(x.directories) - 1; i >= 0; i-- {
		dir := x.directories[i]
		file, err := x.root.OpenFile(dir.name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		err = archiveMetadata(file, dir.mode, dir.time)
		if err = errors.Join(err, file.Close()); err != nil {
			return fmt.Errorf("restore archive directory metadata: %w", err)
		}
	}
	return nil
}
