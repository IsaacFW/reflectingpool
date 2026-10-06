// Package safefs opens folders and files without following a symbolic link
// anywhere below a trusted starting point.
//
// What is on the pool was put there by whoever can write to it. A program
// that opens "<share>/.reflection/notes" by path lets the kernel follow any
// link along the way, so a share writer can send that program's reads and
// writes anywhere the program itself can reach. Here a folder is opened by
// one name at a time, refusing links, and every further step is taken
// relative to the open folder. Nothing swapped in afterwards changes where a
// step goes, because the kernel resolves it against the folder already held.
package safefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Why says what was wrong with a place that was refused.
type Why int

const (
	IsLink     Why = iota // a symbolic link where a folder or file was expected
	NotRegular            // a pipe, a device, a socket, or a folder where a file was expected
	ManyNames             // a file with more than one name: appending to it writes into whatever else it is
)

// Refused reports a place that is not what it should be. It is the sign
// that someone has put something of their own where this program keeps its
// files; the program does not go on.
type Refused struct {
	Path string
	Why  Why
}

func (e *Refused) Error() string { return e.Path + " " + e.Reason() }

// Reason is the problem in words, without the path.
func (e *Refused) Reason() string {
	switch e.Why {
	case IsLink:
		return "is a symbolic link, which is not followed"
	case ManyNames:
		return "has more than one name (a hard link)"
	}
	return "is not an ordinary file"
}

// Dir is an open folder.
type Dir struct {
	fd   int
	path string // for messages only; nothing is opened through it
}

// Open opens a trusted folder, following links: the scan root the owner
// mapped into the container, or a folder the program itself made.
func Open(path string) (*Dir, error) {
	fd, err := retry(func() (int, error) { return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0) })
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return &Dir{fd: fd, path: path}, nil
}

// Close releases the folder.
func (d *Dir) Close() error { return unix.Close(d.fd) }

// Path is where the folder was opened, for messages.
func (d *Dir) Path() string { return d.path }

func (d *Dir) join(name string) string { return strings.TrimSuffix(d.path, "/") + "/" + name }

// plain accepts one name inside the folder: no separators, not "." or "..".
func plain(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "/") && !strings.ContainsRune(name, 0)
}

// Sub opens the folder of the given name inside d. A link is refused.
func (d *Dir) Sub(name string) (*Dir, error) {
	if !plain(name) {
		return nil, &fs.PathError{Op: "open", Path: d.join(name), Err: fs.ErrInvalid}
	}
	fd, err := retry(func() (int, error) {
		return unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	})
	switch {
	case err == unix.ELOOP || err == unix.ENOTDIR:
		// A link to a folder is reported as ENOTDIR, like a plain file; the
		// entry itself says which it is.
		why := NotRegular
		if st, lerr := d.Lstat(name); lerr == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			why = IsLink
		}
		return nil, &Refused{Path: d.join(name), Why: why}
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: d.join(name), Err: err}
	}
	return &Dir{fd: fd, path: d.join(name)}, nil
}

// Walk opens the folder at rel below d, one component at a time. With an
// empty rel it returns a second handle on d itself.
func (d *Dir) Walk(rel string) (*Dir, error) {
	cur, owned := d, false
	for _, part := range strings.Split(rel, "/") {
		if part == "" {
			continue
		}
		next, err := cur.Sub(part)
		if owned {
			cur.Close()
		}
		if err != nil {
			return nil, err
		}
		cur, owned = next, true
	}
	if owned {
		return cur, nil
	}
	fd, err := unix.FcntlInt(uintptr(d.fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "dup", Path: d.path, Err: err}
	}
	return &Dir{fd: fd, path: d.path}, nil
}

// Mkdir makes a folder of the given name inside d and reports whether it
// did. Something already there is left alone; Sub checks it when it is
// opened.
func (d *Dir) Mkdir(name string, mode fs.FileMode) (created bool, err error) {
	if !plain(name) {
		return false, &fs.PathError{Op: "mkdir", Path: d.join(name), Err: fs.ErrInvalid}
	}
	err = unix.Mkdirat(d.fd, name, uint32(mode.Perm()))
	switch {
	case err == unix.EEXIST:
		return false, nil
	case err != nil:
		return false, &fs.PathError{Op: "mkdir", Path: d.join(name), Err: err}
	}
	return true, nil
}

// Stat describes the folder itself.
func (d *Dir) Stat() (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err != nil {
		return st, &fs.PathError{Op: "stat", Path: d.path, Err: err}
	}
	return st, nil
}

// Lstat describes one entry of the folder without following a link.
func (d *Dir) Lstat(name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if !plain(name) {
		return st, &fs.PathError{Op: "lstat", Path: d.join(name), Err: fs.ErrInvalid}
	}
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, &fs.PathError{Op: "lstat", Path: d.join(name), Err: err}
	}
	return st, nil
}

// OpenFile opens an ordinary file of the given name inside d. A link, a
// pipe, a device or anything else that is not an ordinary file is refused.
// With single set, so is a file that has more than one name: appending to
// it would write into whatever else that name belongs to.
func (d *Dir) OpenFile(name string, flag int, perm fs.FileMode, single bool) (*os.File, error) {
	if !plain(name) {
		return nil, &fs.PathError{Op: "open", Path: d.join(name), Err: fs.ErrInvalid}
	}
	// O_NONBLOCK so that a pipe which took the file's place cannot hang the
	// open; it is cleared once the file is known to be ordinary.
	flag |= unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	fd, err := retry(func() (int, error) { return unix.Openat(d.fd, name, flag, uint32(perm.Perm())) })
	switch {
	case err == unix.ELOOP:
		return nil, &Refused{Path: d.join(name), Why: IsLink}
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: d.join(name), Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, &fs.PathError{Op: "stat", Path: d.join(name), Err: err}
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		unix.Close(fd)
		return nil, &Refused{Path: d.join(name), Why: NotRegular}
	case single && st.Nlink > 1:
		unix.Close(fd)
		return nil, &Refused{Path: d.join(name), Why: ManyNames}
	}
	unix.SetNonblock(fd, false)
	return os.NewFile(uintptr(fd), d.join(name)), nil
}

// CreateTemp makes a new file with a name of its own inside d, for writing
// something that is then renamed into place.
func (d *Dir) CreateTemp(prefix string, perm fs.FileMode) (*os.File, error) {
	for range 100 {
		var b [6]byte
		rand.Read(b[:])
		f, err := d.OpenFile(prefix+hex.EncodeToString(b[:]), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm, false)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, &fs.PathError{Op: "create", Path: d.join(prefix + "*"), Err: fs.ErrExist}
}

// Rename gives the entry oldName the name newName, both inside d, replacing
// whatever had that name.
func (d *Dir) Rename(oldName, newName string) error {
	if !plain(oldName) || !plain(newName) {
		return &fs.PathError{Op: "rename", Path: d.join(newName), Err: fs.ErrInvalid}
	}
	if err := unix.Renameat(d.fd, oldName, d.fd, newName); err != nil {
		return &fs.PathError{Op: "rename", Path: d.join(newName), Err: err}
	}
	return nil
}

// Remove deletes the entry of the given name inside d. A link is removed,
// never what it points to.
func (d *Dir) Remove(name string) error {
	if !plain(name) {
		return &fs.PathError{Op: "remove", Path: d.join(name), Err: fs.ErrInvalid}
	}
	if err := unix.Unlinkat(d.fd, name, 0); err != nil {
		return &fs.PathError{Op: "remove", Path: d.join(name), Err: err}
	}
	return nil
}

// Chmod sets the folder's permissions.
func (d *Dir) Chmod(mode fs.FileMode) error { return unix.Fchmod(d.fd, uint32(mode.Perm())) }

// Chown sets the folder's owner.
func (d *Dir) Chown(uid, gid int) error { return unix.Fchown(d.fd, uid, gid) }

func retry(call func() (int, error)) (int, error) {
	for {
		fd, err := call()
		if err != unix.EINTR {
			return fd, err
		}
	}
}
