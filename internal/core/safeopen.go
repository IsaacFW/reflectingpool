package core

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// openFile opens a regular file that lies under one of the scan roots.
//
// Paths come from the index, which was built without following symlinks, but
// the tree can change after a scan. Opening one path component at a time with
// O_NOFOLLOW means a directory later swapped for a symlink cannot lead the
// open outside the scan root.
func (a *App) openFile(path string) (*os.File, error) {
	for _, root := range a.cfg.Roots {
		rel, ok := strings.CutPrefix(path, strings.TrimSuffix(root, "/")+"/")
		if !ok {
			continue
		}
		return openBeneath(root, rel)
	}
	return nil, errors.New("path is outside the scan roots")
}

func openBeneath(root, rel string) (*os.File, error) {
	dirfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { unix.Close(dirfd) }()

	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("invalid path")
		}
		if i < len(parts)-1 {
			next, err := unix.Openat(dirfd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return nil, pathError(err)
			}
			unix.Close(dirfd)
			dirfd = next
			continue
		}
		// O_NONBLOCK so that a FIFO which took the file's place cannot hang
		// the open. O_NOATIME keeps our own reads from making a file look
		// recently used; it is refused for files we do not own.
		const flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		fd, err := unix.Openat(dirfd, part, flags|unix.O_NOATIME, 0)
		if err == unix.EPERM {
			fd, err = unix.Openat(dirfd, part, flags, 0)
		}
		if err != nil {
			return nil, pathError(err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			unix.Close(fd)
			return nil, ErrNotFile
		}
		unix.SetNonblock(fd, false)
		return os.NewFile(uintptr(fd), root+"/"+rel), nil
	}
	return nil, errors.New("invalid path")
}

func pathError(err error) error {
	switch err {
	case unix.ENOENT, unix.ENOTDIR, unix.ELOOP:
		// ELOOP: a symlink now stands where a real file or directory was.
		return ErrGone
	}
	return err
}
