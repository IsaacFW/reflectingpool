package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// threadNice returns a thread's CPU niceness. The raw system call reports
// 20 minus the nice value.
func threadNice(t *testing.T, tid int) int {
	t.Helper()
	prio, err := unix.Getpriority(unix.PRIO_PROCESS, tid)
	if err != nil {
		t.Fatal(err)
	}
	return 20 - prio
}

func threadIOClass(tid int) int {
	v, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
	if errno != 0 {
		return -1
	}
	return int(v) >> ioprioClassShift
}

// niceThreads counts this process's threads running at a lowered priority.
func niceThreads(t *testing.T) int {
	t.Helper()
	tasks, err := filepath.Glob("/proc/self/task/*/stat")
	if err != nil {
		t.Fatal(err)
	}
	lowered := 0
	for _, path := range tasks {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // the thread exited between the listing and the read
		}
		// Fields after the parenthesised name start at field 3; nice is field 19.
		rest := string(data[strings.LastIndexByte(string(data), ')')+1:])
		fields := strings.Fields(rest)
		if len(fields) > 16 {
			if nice, _ := strconv.Atoi(fields[16]); nice != 0 {
				lowered++
			}
		}
	}
	return lowered
}

func TestParseIntensity(t *testing.T) {
	for in, want := range map[string]Intensity{
		"aggressive": Aggressive, "Balanced": Balanced, "low": LowImpact,
		"low impact": LowImpact, "low-impact": LowImpact, " LOW_IMPACT ": LowImpact,
	} {
		got, err := ParseIntensity(in)
		if err != nil || got != want {
			t.Errorf("ParseIntensity(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "fast", "lowest"} {
		if _, err := ParseIntensity(in); err == nil {
			t.Errorf("ParseIntensity(%q) accepted", in)
		}
	}
	for _, i := range []Intensity{Aggressive, Balanced, LowImpact} {
		if back, err := ParseIntensity(i.String()); err != nil || back != i {
			t.Errorf("%v does not round-trip", i)
		}
	}
}

func TestRestScalesWithTheWorkDone(t *testing.T) {
	low, balanced := LowImpact.profile(), Balanced.profile()
	if d := Aggressive.profile().rest(time.Second); d != 0 {
		t.Errorf("an aggressive scan rested %s", d)
	}
	if d := low.rest(10 * time.Millisecond); d != 30*time.Millisecond {
		t.Errorf("low impact rested %s after 10ms of work, want 30ms", d)
	}
	if d := balanced.rest(10 * time.Millisecond); d != 5*time.Millisecond {
		t.Errorf("balanced rested %s after 10ms of work, want 5ms", d)
	}
	// Slower reads, as when something else is using the disks, mean longer rests.
	if low.rest(400*time.Millisecond) <= low.rest(40*time.Millisecond) {
		t.Error("rest does not grow when reads slow down")
	}
	if d := low.rest(time.Minute); d != low.maxRest {
		t.Errorf("rest not capped: %s", d)
	}
	// Work answered from memory costs the disks nothing and earns no rest.
	if d := low.rest(50 * time.Microsecond); d != 0 {
		t.Errorf("rested %s after cached work", d)
	}
}

func TestIntensitySetsWalkersAndThreadPriority(t *testing.T) {
	root := t.TempDir()
	for d := range 5 {
		for f := range 20 {
			write(t, filepath.Join(root, fmt.Sprintf("dir%d/file%d", d, f)), 10)
		}
	}
	type observed struct{ nice, ioClass int }
	entries := -1
	for _, tc := range []struct {
		intensity Intensity
		walkers   int
		want      observed
	}{
		{Aggressive, 3, observed{0, 0}},
		{Balanced, 4, observed{10, ioClassBestEffort}},
		{LowImpact, 1, observed{19, ioClassIdle}},
	} {
		var mu sync.Mutex
		var seen []observed
		sink := &memSink{}
		_, err := Walk(context.Background(), Options{
			Roots: []string{root}, Workers: 3, Intensity: tc.intensity,
			onWalkerStart: func() {
				tid := unix.Gettid()
				mu.Lock()
				seen = append(seen, observed{threadNice(t, tid), threadIOClass(tid)})
				mu.Unlock()
			},
		}, sink)
		if err != nil {
			t.Fatal(err)
		}
		if len(seen) != tc.walkers {
			t.Errorf("%v: %d walkers, want %d", tc.intensity, len(seen), tc.walkers)
		}
		for _, got := range seen {
			if got != tc.want {
				t.Errorf("%v: walker thread nice=%d io class=%d, want %+v", tc.intensity, got.nice, got.ioClass, tc.want)
			}
		}
		// Every intensity must find exactly the same tree.
		if entries == -1 {
			entries = len(sink.entries)
		} else if len(sink.entries) != entries {
			t.Errorf("%v found %d entries, aggressive found %d", tc.intensity, len(sink.entries), entries)
		}
	}

	// The lowered priority must die with the walkers. If it stayed on a
	// thread Go reuses, the web server would end up running at it.
	deadline := time.Now().Add(2 * time.Second)
	for niceThreads(t) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d threads still run at lowered priority after the scans finished", niceThreads(t))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLowImpactRestsBetweenChunks(t *testing.T) {
	root := t.TempDir()
	// More than two read chunks in one folder, so rests must happen inside
	// the folder and not only after it.
	for f := range 2*readChunk + 100 {
		write(t, filepath.Join(root, "big", fmt.Sprintf("f%05d", f)), 0)
	}
	run := func(intensity Intensity) (rests int, prog *Progress) {
		prog = &Progress{}
		_, err := Walk(context.Background(), Options{
			Roots: []string{root}, Intensity: intensity, Progress: prog,
			sleep: func(time.Duration) { rests++ }, // one walker in low impact, so no lock needed
		}, &memSink{})
		if err != nil {
			t.Fatal(err)
		}
		return rests, prog
	}
	rests, prog := run(LowImpact)
	if rests < 2 || prog.Rested.Load() <= 0 {
		t.Errorf("low impact rested %d times (%s) reading a folder of %d files", rests, time.Duration(prog.Rested.Load()), 2*readChunk+100)
	}
	var aggressiveRests int
	prog = &Progress{}
	if _, err := Walk(context.Background(), Options{
		Roots: []string{root}, Workers: 1, Progress: prog,
		sleep: func(time.Duration) { aggressiveRests++ },
	}, &memSink{}); err != nil {
		t.Fatal(err)
	}
	if aggressiveRests != 0 || prog.Rested.Load() != 0 {
		t.Errorf("an aggressive scan rested %d times", aggressiveRests)
	}
}

func TestRestIsCutShortWhenTheScanStops(t *testing.T) {
	w := &walker{prof: profile{restRatio: 1000, maxRest: time.Minute}, prog: &Progress{}, quit: make(chan struct{})}
	close(w.quit)
	start := time.Now()
	w.rest(time.Second) // would be a one-minute rest
	if took := time.Since(start); took > time.Second {
		t.Errorf("a stopped scan kept resting for %s", took)
	}
}
