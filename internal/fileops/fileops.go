// Package fileops executes file-manager operations against a descriptor-held
// data directory. No user-controlled filesystem operation is delegated to a shell.
package fileops

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type operationError struct {
	code    int
	message string
}

func (e *operationError) Error() string    { return e.message }
func bad(format string, args ...any) error { return &operationError{4, fmt.Sprintf(format, args...)} }
func conflict(format string, args ...any) error {
	return &operationError{7, fmt.Sprintf(format, args...)}
}

// Run accepts ROOT OP ARGS... and returns the remote exec exit status. Input is
// drained even on refusal: Kubernetes exec waits for its stdin stream to finish.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	defer io.Copy(io.Discard, stdin)
	if len(args) < 2 {
		fmt.Fprintln(stderr, "expected ROOT OP ARGS...")
		return 4
	}
	r, err := os.OpenRoot(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "the data directory is not available:", err)
		return 5
	}
	defer r.Close()
	err = execute(r, args[1], args[2:], stdin, stdout)
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, err)
	var op *operationError
	if errors.As(err, &op) {
		return op.code
	}
	if errors.Is(err, os.ErrNotExist) {
		return 6
	}
	if rootEscape(err) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) {
		return 4
	}
	return 1
}

// os.Root's escape sentinel is unexported. Match only an error leaf, never a
// message containing an attacker-controlled filename; joined close errors are
// traversed as well as ordinary PathError wrappers.
func rootEscape(err error) bool {
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range many.Unwrap() {
			if rootEscape(child) {
				return true
			}
		}
		return false
	}
	if child := errors.Unwrap(err); child != nil {
		return rootEscape(child)
	}
	return err != nil && err.Error() == "path escapes from parent"
}

func relative(r *os.Root, name string) (string, error) {
	if !filepath.IsAbs(name) {
		return "", bad("expected an absolute data path")
	}
	rel, err := filepath.Rel(r.Name(), name)
	if err != nil || !filepath.IsLocal(rel) {
		return "", bad("path escapes the data directory")
	}
	return rel, nil
}

func execute(r *os.Root, op string, args []string, in io.Reader, out io.Writer) error {
	min, max, paths := 1, 1, 1
	switch op {
	case "list", "read", "archive", "mkdir", "upload-size", "upload-remove":
	case "delete":
		max, paths = -1, -1
	case "write", "extract", "decompress", "upload-check", "upload-append":
		min, max = 2, 2
	case "rename":
		min, max, paths = 2, 2, 2
	case "move", "copy":
		min, max, paths = 2, -1, -1
	case "compress":
		min, max = 3, -1
	case "upload-finish-file":
		min, max, paths = 3, 3, 2
	case "upload-finish-archive":
		min, max, paths = 4, 4, 2
	default:
		return bad("unknown file operation %q", op)
	}
	if len(args) < min || (max >= 0 && len(args) != max) {
		return bad("invalid arguments for %s", op)
	}
	a := append([]string(nil), args...)
	if paths == -1 {
		paths = len(a)
	}
	for i := range paths {
		name, err := relative(r, a[i])
		if err != nil {
			return err
		}
		deref := true
		switch op {
		case "archive", "write", "delete", "rename", "copy", "upload-finish-file", "upload-remove":
			deref = false
		case "move":
			deref = i == 0
		}
		a[i], err = Resolve(r, name, deref)
		if err != nil {
			return err
		}
	}
	switch op {
	case "list":
		return list(r, a[0], out)
	case "read":
		f, err := regular(r, a[0], os.O_RDONLY)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(out, f)
		return err
	case "write":
		return atomicWrite(r, a[0], a[1], in)
	case "mkdir":
		return r.MkdirAll(a[0], 0755)
	case "delete":
		for _, name := range a {
			if name == "." {
				return bad("refusing to delete the data root")
			}
			if err := r.RemoveAll(name); err != nil {
				return err
			}
		}
		return nil
	case "rename":
		return renameNew(r, a[0], a[1])
	case "move":
		dir, err := r.OpenRoot(a[0])
		if err != nil {
			return err
		}
		dir.Close()
		for _, src := range a[1:] {
			if _, err := r.Lstat(src); err != nil {
				return err
			}
			if err := available(r, filepath.Join(a[0], filepath.Base(src))); err != nil {
				return err
			}
		}
		for _, src := range a[1:] {
			if err := renameNew(r, src, filepath.Join(a[0], filepath.Base(src))); err != nil {
				return err
			}
		}
		return nil
	case "copy":
		if _, err := r.Lstat(a[0]); err != nil {
			return err
		}
		for _, dst := range a[1:] {
			if _, err := r.Lstat(dst); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := copyEntry(r, a[0], dst); err != nil {
				return err
			}
			_, err := io.WriteString(out, filepath.Join(r.Name(), dst))
			return err
		}
		return bad("no free name for the copy")
	case "archive":
		base := filepath.Base(a[0])
		if a[0] == "." {
			base = filepath.Base(r.Name())
		}
		return writeArchive(r, []string{a[0]}, []string{base}, out)
	case "compress":
		if !plainName(a[1]) {
			return bad("invalid archive name")
		}
		for _, name := range a[2:] {
			if !plainName(name) {
				return bad("invalid archive member name")
			}
		}
		dir, err := r.OpenRoot(a[0])
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := available(dir, a[1]); err != nil {
			return err
		}
		f, tmp, err := temporary(dir)
		if err != nil {
			return err
		}
		defer dir.Remove(tmp)
		names := make([]string, len(a)-2)
		for i, name := range a[2:] {
			names[i] = filepath.Join(a[0], name)
		}
		err = writeArchive(r, names, a[2:], f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err := renameNew(dir, tmp, a[1]); err != nil {
			return err
		}
		_, err = io.WriteString(out, a[1])
		return err
	case "extract":
		if err := r.MkdirAll(a[0], 0755); err != nil {
			return err
		}
		dir, err := r.OpenRoot(a[0])
		if err != nil {
			return err
		}
		defer dir.Close()
		f, tmp, err := temporary(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		defer dir.Remove(tmp)
		if _, err := io.Copy(f, in); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return extractArchive(r, a[0], f, a[1])
	case "decompress":
		f, err := regular(r, a[0], os.O_RDONLY)
		if err != nil {
			return err
		}
		defer f.Close()
		return extractArchive(r, filepath.Dir(a[0]), f, a[1])
	case "upload-check":
		if a[1] == "archive" {
			return r.MkdirAll(a[0], 0755)
		}
		parent, err := r.OpenRoot(filepath.Dir(a[0]))
		if err != nil {
			return err
		}
		parent.Close()
		fi, err := r.Stat(a[0])
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return bad("is not a regular file")
		}
		return nil
	case "upload-size":
		n, err := size(r, a[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(out, n)
		return err
	case "upload-append":
		want, err := count(a[1])
		if err != nil {
			return err
		}
		f, err := regular(r, a[0], os.O_WRONLY|os.O_CREATE|os.O_APPEND)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if fi.Size() != want {
			fmt.Fprint(out, fi.Size())
			return conflict("the upload holds %d bytes, not %d", fi.Size(), want)
		}
		n, err := io.Copy(f, in)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		_, err = fmt.Fprint(out, want+n)
		return err
	case "upload-finish-file", "upload-finish-archive":
		want, err := count(a[2])
		if err != nil {
			return err
		}
		f, err := regular(r, a[0], os.O_RDONLY)
		if os.IsNotExist(err) {
			return conflict("the upload holds 0 of %d bytes", want)
		}
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if fi.Size() != want {
			return conflict("the upload holds %d of %d bytes", fi.Size(), want)
		}
		if op == "upload-finish-file" {
			return r.Rename(a[0], a[1])
		}
		if err := r.MkdirAll(a[1], 0755); err != nil {
			return err
		}
		err = extractArchive(r, a[1], f, a[3])
		removeErr := r.Remove(a[0])
		if err != nil {
			return err
		}
		return removeErr
	case "upload-remove":
		err := r.Remove(a[0])
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return bad("unknown operation")
}

func plainName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}
func count(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, bad("invalid byte count")
	}
	return n, nil
}

func regular(r *os.Root, name string, flags int) (*os.File, error) {
	f, err := r.OpenFile(name, flags|syscall.O_NONBLOCK, 0644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, bad("not a regular file")
	}
	return f, nil
}

func size(r *os.Root, name string) (int64, error) {
	f, err := regular(r, name, os.O_RDONLY)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func temporary(r *os.Root) (*os.File, string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, "", err
	}
	name := ".quetzal-part-" + hex.EncodeToString(random[:])
	f, err := r.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
	return f, name, err
}

func atomicWrite(r *os.Root, dst, expected string, in io.Reader) error {
	// Hold the parent as well as the volume. Neither the spool nor the final
	// rename can be redirected while a slow request is sending its body.
	parent, err := r.OpenRoot(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer parent.Close()
	f, tmp, err := temporary(parent)
	if err != nil {
		return err
	}
	defer parent.Remove(tmp)
	n, err := io.Copy(f, in)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if expected != "" {
		want, err := count(expected)
		if err != nil {
			return err
		}
		if n != want {
			return fmt.Errorf("short write: received %d of %d bytes", n, want)
		}
	}
	return parent.Rename(tmp, filepath.Base(dst))
}

func available(r *os.Root, name string) error {
	_, err := r.Lstat(name)
	if err == nil {
		return bad("%s already exists", filepath.Base(name))
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// renameNew pins both parents and uses an atomic no-replace rename. A check
// followed by os.Rename could otherwise clobber a concurrently created entry.
func renameNew(r *os.Root, from, to string) error {
	if from == "." || to == "." {
		return bad("cannot rename the data root")
	}
	if err := available(r, to); err != nil {
		return err
	}
	src, err := r.OpenFile(filepath.Dir(from), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := r.OpenFile(filepath.Dir(to), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer dst.Close()
	err = unix.Renameat2(int(src.Fd()), filepath.Base(from), int(dst.Fd()), filepath.Base(to), unix.RENAME_NOREPLACE)
	if errors.Is(err, syscall.EEXIST) {
		return bad("%s already exists", filepath.Base(to))
	}
	return err
}

func list(r *os.Root, name string, out io.Writer) error {
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		p := filepath.Join(name, entry.Name())
		fi, err := r.Stat(p)
		if err != nil {
			fi, err = r.Lstat(p)
		}
		if err != nil {
			continue
		}
		kind, size := "f", fi.Size()
		if fi.IsDir() {
			kind, size = "d", 0
		}
		if _, err := fmt.Fprintf(out, "%s\t%d\t%d\t%s\x00", kind, size, fi.ModTime().Unix(), entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func copyEntry(r *os.Root, src, dst string) error {
	fi, err := r.Lstat(src)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := r.Readlink(src)
		if err != nil {
			return err
		}
		return r.Symlink(target, dst)
	}
	if fi.IsDir() {
		if err := r.Mkdir(dst, fi.Mode().Perm()|0700); err != nil {
			return err
		}
		f, err := r.OpenFile(src, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyEntry(r, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		dir, err := r.OpenFile(dst, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := dir.Chmod(fi.Mode().Perm()); err != nil {
			return err
		}
		return fileTimes(dir, fi.ModTime())
	}
	if !fi.Mode().IsRegular() {
		return bad("cannot copy a special file")
	}
	in, err := regular(r, src, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := r.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(fi.Mode().Perm())
	}
	if err == nil {
		err = fileTimes(out, fi.ModTime())
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}
