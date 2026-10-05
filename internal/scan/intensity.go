package scan

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Intensity trades how fast a scan finishes against how much it disturbs
// other work on the same disks.
type Intensity int

const (
	// Aggressive walks with every walker at normal priority. It is the
	// fastest and, on a busy pool, takes about half the disks' attention.
	Aggressive Intensity = iota
	// Balanced uses a few walkers at reduced priority with short rests.
	Balanced
	// LowImpact uses one walker at the lowest priority and rests for longer
	// than it works, backing off further when the disks are slow to answer.
	LowImpact
)

func (i Intensity) String() string {
	switch i {
	case Balanced:
		return "balanced"
	case LowImpact:
		return "low"
	}
	return "aggressive"
}

// ParseIntensity accepts "aggressive", "balanced" and "low" (or "low impact").
func ParseIntensity(s string) (Intensity, error) {
	switch strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(s))) {
	case "aggressive":
		return Aggressive, nil
	case "balanced":
		return Balanced, nil
	case "low", "lowimpact":
		return LowImpact, nil
	}
	return Aggressive, fmt.Errorf("unknown scan intensity %q: use aggressive, balanced or low", s)
}

const (
	ioprioWhoProcess  = 1
	ioprioClassShift  = 13
	ioClassBestEffort = 2
	ioClassIdle       = 3

	// A chunk of work this short was answered from memory; resting after
	// it would slow the scan without sparing any disk.
	minRest = 200 * time.Microsecond
)

// profile is what an intensity means in practice.
type profile struct {
	workers int // 0 leaves Options.Workers in charge
	nice    int // CPU niceness for walker threads; 0 leaves them alone
	ioClass int // I/O priority class for walker threads; 0 leaves them alone
	ioLevel int
	// restRatio is how long a walker rests after a chunk of work, as a
	// multiple of how long the chunk took. Because the rest scales with
	// the time taken, a walker slowed by competing disk activity rests
	// longer: the scan yields without having to measure anyone else.
	restRatio float64
	maxRest   time.Duration
}

func (i Intensity) profile() profile {
	switch i {
	case Balanced:
		return profile{workers: 4, nice: 10, ioClass: ioClassBestEffort, ioLevel: 7, restRatio: 0.5, maxRest: time.Second}
	case LowImpact:
		// Rest three times as long as each read took: the scan asks for at
		// most a quarter of one disk's time.
		return profile{workers: 1, nice: 19, ioClass: ioClassIdle, restRatio: 3, maxRest: 5 * time.Second}
	}
	return profile{}
}

func (p profile) rest(worked time.Duration) time.Duration {
	if p.restRatio <= 0 || worked < minRest {
		return 0
	}
	return min(time.Duration(float64(worked)*p.restRatio), p.maxRest)
}

// runWalker runs f, a walker's whole life, on an OS thread whose CPU and I/O
// priority match the profile.
//
// An unprivileged process can lower a thread's priority but not raise it
// again, so the lowered thread must not outlive the walker. The goroutine
// stays locked to the thread and never unlocks it: Go destroys a locked
// thread when its goroutine exits, which keeps the lowered priority from
// reaching the web server's goroutines. Go cannot destroy the process's main
// thread, though, so a walker that lands there holds it untouched and runs
// on a fresh thread.
//
// Setting the priorities is best effort. The I/O class matters on filesystems
// that use the kernel's block scheduler; ZFS schedules its own I/O and
// ignores it, which is why the scan also rests between reads.
func (p profile) runWalker(f func()) {
	if p.nice == 0 && p.ioClass == 0 {
		f()
		return
	}
	runtime.LockOSThread()
	tid := unix.Gettid()
	if tid == unix.Getpid() {
		// While this goroutine holds the main thread, the one started
		// here cannot be scheduled onto it.
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.runWalker(f)
		}()
		<-done
		runtime.UnlockOSThread()
		return
	}
	if p.nice != 0 {
		unix.Setpriority(unix.PRIO_PROCESS, tid, p.nice)
	}
	if p.ioClass != 0 {
		unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), uintptr(p.ioClass<<ioprioClassShift|p.ioLevel))
	}
	f()
}
