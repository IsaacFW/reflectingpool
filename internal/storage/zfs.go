package storage

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ZFSDataset is one row of `zfs list`. Sizes are bytes; -1 means the property
// does not apply to this row, as with a snapshot's available space.
type ZFSDataset struct {
	Name          string  `json:"name"`
	Type          string  `json:"type"` // filesystem, volume or snapshot
	Used          int64   `json:"used"`
	Avail         int64   `json:"avail"`
	Refer         int64   `json:"refer"`
	UsedSnap      int64   `json:"used_by_snapshots"`
	UsedDataset   int64   `json:"used_by_dataset"`
	UsedChildren  int64   `json:"used_by_children"`
	CompressRatio float64 `json:"compress_ratio"`
	Mountpoint    string  `json:"mountpoint"`
	Atime         string  `json:"atime"`
	Relatime      string  `json:"relatime"`
	Creation      int64   `json:"creation"`
}

// ZFSListArgs is the one command this program ever runs against ZFS. It only
// reads. The host-script alternative must run exactly this, so both paths
// share a parser:
//
//	zfs list -Hp -t filesystem,volume,snapshot -o name,type,used,avail,refer,usedsnap,usedds,usedchild,compressratio,mountpoint,atime,relatime,creation
var ZFSListArgs = []string{
	"list", "-Hp", "-t", "filesystem,volume,snapshot",
	"-o", "name,type,used,avail,refer,usedsnap,usedds,usedchild,compressratio,mountpoint,atime,relatime,creation",
}

const zfsListFields = 13

// ParseZFSList parses the tab-separated output of `zfs` with ZFSListArgs.
func ParseZFSList(r io.Reader) ([]ZFSDataset, error) {
	var out []ZFSDataset
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != zfsListFields {
			return nil, fmt.Errorf("zfs list: expected %d fields, got %d in %q", zfsListFields, len(f), line)
		}
		num := func(s string) int64 {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return -1
			}
			return v
		}
		text := func(s string) string {
			if s == "-" {
				return ""
			}
			return s
		}
		ratio, _ := strconv.ParseFloat(strings.TrimSuffix(f[8], "x"), 64)
		out = append(out, ZFSDataset{
			Name: f[0], Type: f[1],
			Used: num(f[2]), Avail: num(f[3]), Refer: num(f[4]),
			UsedSnap: num(f[5]), UsedDataset: num(f[6]), UsedChildren: num(f[7]),
			CompressRatio: ratio,
			Mountpoint:    text(f[9]), Atime: text(f[10]), Relatime: text(f[11]),
			Creation: num(f[12]),
		})
	}
	return out, sc.Err()
}

// ZFSListing is the result of asking ZFS, by whichever route worked.
type ZFSListing struct {
	Datasets []ZFSDataset
	// Origin is "zfs" when the command ran, "file" when a host script's
	// output was read, and "none" when neither is available.
	Origin string
	AsOf   time.Time
	// Err explains why a route that looked available did not work.
	Err string
}

// ListZFS reads dataset and snapshot accounting. It tries the zfs command when
// /dev/zfs is present, then listFile, and reports "none" rather than failing:
// everything but snapshot sizes works without it.
func ListZFS(ctx context.Context, listFile string) ZFSListing {
	var res ZFSListing
	if _, err := os.Stat("/dev/zfs"); err == nil {
		if bin, err := exec.LookPath("zfs"); err == nil {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			var stdout, stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, bin, ZFSListArgs...)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if err == nil {
				if ds, perr := ParseZFSList(&stdout); perr == nil {
					return ZFSListing{Datasets: ds, Origin: "zfs", AsOf: time.Now()}
				} else {
					err = perr
				}
			}
			res.Err = strings.TrimSpace(fmt.Sprintf("zfs list: %v %s", err, firstLine(stderr.String())))
		} else {
			res.Err = "/dev/zfs is present but the zfs command is not installed"
		}
	}
	if listFile != "" {
		if f, err := os.Open(listFile); err == nil {
			defer f.Close()
			ds, perr := ParseZFSList(f)
			if perr == nil {
				res.Datasets, res.Origin = ds, "file"
				if st, err := f.Stat(); err == nil {
					res.AsOf = st.ModTime()
				}
				return res
			}
			res.Err = strings.TrimSpace(res.Err + " " + listFile + ": " + perr.Error())
		}
	}
	res.Origin = "none"
	return res
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
