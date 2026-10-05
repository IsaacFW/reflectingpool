// Package storage reads what the operating system and ZFS know about the
// filesystems being scanned: mounts, free space and dataset accounting.
package storage

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	Dev         uint64
	Root        string // path within the filesystem that is mounted; "/" for all of it
	MountPoint  string
	Options     string
	Propagation string // optional fields such as "shared:1" or "master:2"
	FSType      string
	Source      string // for ZFS, the dataset name
}

// Mounts reads the mounts visible to this process.
func Mounts() ([]Mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMountinfo(f)
}

// ParseMountinfo parses the format documented in proc_pid_mountinfo(5):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
func ParseMountinfo(r io.Reader) ([]Mount, error) {
	var out []Mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		left, right, ok := strings.Cut(line, " - ")
		lf, rf := strings.Fields(left), strings.Fields(right)
		if !ok || len(lf) < 6 || len(rf) < 2 {
			return nil, fmt.Errorf("mountinfo: malformed line %q", line)
		}
		maj, min, ok := strings.Cut(lf[2], ":")
		major, err1 := strconv.ParseUint(maj, 10, 32)
		minor, err2 := strconv.ParseUint(min, 10, 32)
		if !ok || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("mountinfo: bad device %q", lf[2])
		}
		out = append(out, Mount{
			Dev:         unix.Mkdev(uint32(major), uint32(minor)),
			Root:        unescape(lf[3]),
			MountPoint:  unescape(lf[4]),
			Options:     lf[5],
			Propagation: strings.Join(lf[6:], " "),
			FSType:      rf[0],
			Source:      unescape(rf[1]),
		})
	}
	return out, sc.Err()
}

// unescape decodes the octal escapes the kernel uses for space, tab, newline
// and backslash in mount paths.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Table answers questions about a set of mounts.
type Table struct {
	all   []Mount
	byDev map[uint64]Mount
}

func NewTable(mounts []Mount) *Table {
	t := &Table{all: mounts, byDev: make(map[uint64]Mount, len(mounts))}
	for _, m := range mounts {
		// One filesystem can be mounted several times; prefer the mount of
		// its root, then the shortest path.
		cur, ok := t.byDev[m.Dev]
		if !ok || (m.Root == "/" && cur.Root != "/") || (m.Root == cur.Root && len(m.MountPoint) < len(cur.MountPoint)) {
			t.byDev[m.Dev] = m
		}
	}
	return t
}

func (t *Table) ByDev(dev uint64) (Mount, bool) {
	m, ok := t.byDev[dev]
	return m, ok
}

// Under lists the mounts at or beneath root, one per filesystem, ordered by
// mount point.
func (t *Table) Under(root string) []Mount {
	var out []Mount
	seen := make(map[uint64]bool)
	for _, m := range t.all {
		if m.MountPoint != root && !strings.HasPrefix(m.MountPoint, strings.TrimSuffix(root, "/")+"/") {
			continue
		}
		if !seen[m.Dev] {
			seen[m.Dev] = true
			out = append(out, m)
		}
	}
	return out
}

// Containing returns the mount that path lives on.
func (t *Table) Containing(path string) (Mount, bool) {
	var best Mount
	found := false
	for _, m := range t.all {
		if m.MountPoint == path || m.MountPoint == "/" || strings.HasPrefix(path, m.MountPoint+"/") {
			// Later lines shadow earlier ones at the same mount point.
			if !found || len(m.MountPoint) >= len(best.MountPoint) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// Usage is what statfs reports for a filesystem.
type Usage struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Avail int64 `json:"avail"`
	// Objects is the number of inodes in use. On ZFS this is the dataset's
	// object count, which is close to its number of files and directories.
	Objects int64 `json:"objects"`
}

func StatUsage(path string) (Usage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bs := int64(st.Bsize)
	return Usage{
		Total:   int64(st.Blocks) * bs,
		Used:    int64(st.Blocks-st.Bfree) * bs,
		Avail:   int64(st.Bavail) * bs,
		Objects: int64(st.Files - st.Ffree),
	}, nil
}
