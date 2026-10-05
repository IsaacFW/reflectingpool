package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const mountinfo = `22 1 0:21 / / rw,relatime - overlay overlay rw,lowerdir=/a
30 22 0:45 / /mnt/tank rw,noatime master:5 - zfs tank rw,xattr,posixacl
31 30 0:46 / /mnt/tank/media rw,noatime master:6 - zfs tank/media rw,xattr,posixacl
32 30 0:47 / /mnt/tank/my\040docs rw,noatime master:7 - zfs tank/my\040docs rw
33 22 0:45 /appdata/rp /data rw,noatime master:5 - zfs tank rw
34 22 8:1 / /mnt/tankard rw - xfs /dev/sda1 rw
`

func TestParseMountinfo(t *testing.T) {
	ms, err := ParseMountinfo(strings.NewReader(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 6 {
		t.Fatalf("got %d mounts", len(ms))
	}
	if m := ms[3]; m.MountPoint != "/mnt/tank/my docs" || m.Source != "tank/my docs" {
		t.Errorf("escapes not decoded: %+v", m)
	}
	if m := ms[1]; m.FSType != "zfs" || m.Source != "tank" || m.Dev != unix.Mkdev(0, 45) || m.Propagation != "master:5" {
		t.Errorf("tank = %+v", m)
	}

	tab := NewTable(ms)
	// /data is a subdirectory bind of the same filesystem; the dataset's own
	// mount must win.
	if m, _ := tab.ByDev(unix.Mkdev(0, 45)); m.MountPoint != "/mnt/tank" {
		t.Errorf("ByDev = %+v", m)
	}
	under := tab.Under("/mnt/tank")
	if len(under) != 3 {
		t.Errorf("Under(/mnt/tank) = %d mounts; /mnt/tankard must not match", len(under))
	}
	if m, ok := tab.Containing("/mnt/tank/media/movies/a.mkv"); !ok || m.Source != "tank/media" {
		t.Errorf("Containing = %+v", m)
	}
	if m, _ := tab.Containing("/mnt/tank/other"); m.Source != "tank" {
		t.Errorf("Containing = %+v", m)
	}

	if _, err := ParseMountinfo(strings.NewReader("garbage\n")); err == nil {
		t.Error("malformed line accepted")
	}
}

const zfsList = "tank\tfilesystem\t26388279066624\t52776558133248\t219902\t0\t219902\t26388278846722\t1.08\t/mnt/tank\toff\ton\t1700000000\n" +
	"tank/media\tfilesystem\t15393162788864\t52776558133248\t14293651161088\t1099511627776\t14293651161088\t0\t1.00\t/mnt/tank/media\toff\ton\t1700000100\n" +
	"tank/media@daily-2026-10-01\tsnapshot\t549755813888\t-\t14000000000000\t-\t-\t-\t1.00\t-\t-\t-\t1790000000\n"

func TestParseZFSList(t *testing.T) {
	ds, err := ParseZFSList(strings.NewReader(zfsList))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 {
		t.Fatalf("got %d rows", len(ds))
	}
	media := ds[1]
	if media.Name != "tank/media" || media.UsedSnap != 1099511627776 || media.Mountpoint != "/mnt/tank/media" || media.Atime != "off" {
		t.Errorf("media = %+v", media)
	}
	if ds[0].CompressRatio != 1.08 {
		t.Errorf("ratio = %v", ds[0].CompressRatio)
	}
	snap := ds[2]
	if snap.Type != "snapshot" || snap.Used != 549755813888 || snap.Avail != -1 || snap.Mountpoint != "" {
		t.Errorf("snapshot = %+v", snap)
	}
	if _, err := ParseZFSList(strings.NewReader("tank\tfilesystem\n")); err == nil {
		t.Error("short row accepted")
	}
}

func TestListZFSFallsBackToFile(t *testing.T) {
	if _, err := os.Stat("/dev/zfs"); err == nil {
		t.Skip("this machine has ZFS")
	}
	file := filepath.Join(t.TempDir(), "zfs-list.txt")
	if got := ListZFS(context.Background(), file); got.Origin != "none" {
		t.Errorf("origin = %q, want none", got.Origin)
	}
	if err := os.WriteFile(file, []byte(zfsList), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ListZFS(context.Background(), file)
	if got.Origin != "file" || len(got.Datasets) != 3 || got.AsOf.IsZero() {
		t.Errorf("listing = %+v", got)
	}
}

func TestStatUsage(t *testing.T) {
	u, err := StatUsage(t.TempDir())
	if err != nil || u.Total <= 0 || u.Used < 0 || u.Avail > u.Total {
		t.Errorf("usage = %+v, %v", u, err)
	}
}

func TestMountsOnThisMachine(t *testing.T) {
	ms, err := Mounts()
	if err != nil || len(ms) == 0 {
		t.Fatalf("Mounts: %d, %v", len(ms), err)
	}
}
