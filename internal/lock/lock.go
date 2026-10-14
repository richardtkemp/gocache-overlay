//go:build linux

// Package lock takes a host-wide flock lazily: on the first cache miss of a
// go command, not when it starts, so a go command the cache fully covers
// never waits for it. The lock is held until the helper (and so the go
// command) exits.
//
// Three cases do not take the lock, because the process tree already holds it
// and waiting would deadlock:
//
//   - $HEAVY_LOCK_HELD names the lock file: a caller that took the lock up
//     front (for example around a go test -count=1 run, which the cache cannot
//     cover) says so for everything it starts;
//   - the holder recorded in /proc/locks is an ancestor of this process;
//   - an ancestor holds the lock through one of its open files: its
//     /proc/<pid>/fdinfo/<fd> for an fd on the lock file has a "lock:" line.
//     /proc/locks names the process that called flock, which is not always
//     the one holding it: in `( flock 9; go build 9<&- ) 9<lockfile` the
//     flock(1) child takes the lock and exits, and the lock stays with the
//     subshell's fd 9, so /proc/locks names a dead pid (#2418);
//   - the holder is another process of this program (same /proc/<pid>/comm)
//     whose parent is an ancestor of this process: a go command run by a test that its own go command's
//     helper is holding the lock for (a nested `go build` in a test).
//
// When it does take the lock it prints the same two lines a build script
// around flock prints (">>> waiting for heavy lock ... [HH:MM:SS]" before
// blocking, ">>> acquired heavy lock [HH:MM:SS]" after), so whatever reads a
// build's output for lock waits sees these too. It prints nothing when it does
// not take the lock.
package lock

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// HeldEnv is the environment variable a caller holding the lock sets for the
// processes it starts: a list of lock paths (colon-separated) it holds.
const HeldEnv = "HEAVY_LOCK_HELD"

// Lazy is a lock taken at most once, on the first call to Need.
type Lazy struct {
	Path string
	// Log, if set, gets the waiting and acquired lines when this process
	// takes the lock (the go command passes the helper's stderr through as
	// its own).
	Log io.Writer

	once  sync.Once
	err   error
	f     *os.File // open and flocked when this process took the lock; kept referenced until exit
	state string
}

// Need makes sure the lock is held for this process tree, taking it if
// needed. It returns an error when the lock file cannot be opened or locked:
// the caller must fail the build rather than run unlocked.
func (l *Lazy) Need() error {
	l.once.Do(func() { l.state, l.err = l.acquire() })
	return l.err
}

// State says how the lock was obtained: "" (never needed), "taken",
// "taken after waiting <d>", or "held by ..." (not taken: see the package
// comment).
func (l *Lazy) State() string { return l.state }

func (l *Lazy) acquire() (string, error) {
	if slices.Contains(filepath.SplitList(os.Getenv(HeldEnv)), l.Path) {
		return "held by caller ($" + HeldEnv + ")", nil
	}
	// Read-only and never created: a sandbox may allow reading it only, and a
	// missing lock file is a setup error, not a reason to run unlocked.
	f, err := os.OpenFile(l.Path, os.O_RDONLY, 0)
	if err != nil {
		return "", fmt.Errorf("lock: %w", err)
	}
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		l.f = f
		// Not blocked, but the pair still marks the take, as a script's
		// unconditional echo-flock-echo does.
		l.say(waitingLine)
		l.say(acquiredLine)
		return "taken", nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return "", fmt.Errorf("lock %s: %w", l.Path, err)
	}
	if who := heldByLineage(f); who != "" {
		f.Close()
		return who, nil
	}
	l.say(waitingLine)
	start := time.Now()
	if err := flockRetry(fd); err != nil {
		f.Close()
		return "", fmt.Errorf("lock %s: %w", l.Path, err)
	}
	l.f = f
	l.say(acquiredLine)
	return fmt.Sprintf("taken after waiting %s", time.Since(start).Round(time.Millisecond)), nil
}

// The lines a build prints around taking the lock: lock-wait readers (the
// factory's timeline charts) match ">>> waiting for heavy lock" and
// ">>> acquired heavy lock", each followed by the first "[HH:MM:SS]" (local
// time). The stamp comes before the path so a '[' in the path cannot shadow it.
const (
	waitingLine  = ">>> waiting for heavy lock ... [%s] (gocache-overlay: %s; another build may be running)\n"
	acquiredLine = ">>> acquired heavy lock [%s] (gocache-overlay: %s)\n"
)

// say prints one of the lines above as a single write, so it is not split by
// the go command's own output on the same stderr.
func (l *Lazy) say(format string) {
	if l.Log != nil {
		fmt.Fprintf(l.Log, format, time.Now().Format(time.TimeOnly), l.Path)
	}
}

func flockRetry(fd int) error {
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// heldByLineage names the holder when the lock is held by this process's
// ancestry (see the package comment), else "".
func heldByLineage(f *os.File) string {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return ""
	}
	anc := ancestors(os.Getpid())
	id := fileID(st.Dev, st.Ino)
	for _, h := range holders(id) {
		if slices.Contains(anc, h) {
			return fmt.Sprintf("held by ancestor %d", h)
		}
		// The same program as this one (compared by name, so a renamed binary still matches its copies).
		if c := comm(h); c != "" && c == comm(os.Getpid()) {
			if p := ppid(h); p > 0 && slices.Contains(anc, p) {
				return fmt.Sprintf("held by %d, the helper of ancestor %d", h, p)
			}
		}
	}
	for _, a := range anc {
		if fd := lockedFD(a, st.Dev, st.Ino, id); fd >= 0 {
			return fmt.Sprintf("held by ancestor %d through its fd %d", a, fd)
		}
	}
	return ""
}

// fileID is a file as /proc/locks and fdinfo "lock:" lines print it: the
// device as %02x:%02x (major:minor) and the inode in decimal.
func fileID(dev, ino uint64) string {
	return fmt.Sprintf("%02x:%02x:%d", major(dev), minor(dev), ino)
}

// lockedFD returns an fd of pid that is open on the file (dev, ino) and holds
// an flock on it, else -1. The kernel lists, in /proc/<pid>/fdinfo/<fd>, the
// locks held through that fd's open file description, whichever process took
// them. Processes this one may not inspect (another user's) are skipped.
func lockedFD(pid int, dev, ino uint64, id string) int {
	dir := "/proc/" + strconv.Itoa(pid)
	ents, err := os.ReadDir(dir + "/fd")
	if err != nil {
		return -1
	}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat(dir+"/fd/"+e.Name(), &st) != nil || st.Dev != dev || st.Ino != ino {
			continue
		}
		b, err := os.ReadFile(dir + "/fdinfo/" + e.Name())
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			// "lock:\t1: FLOCK  ADVISORY  WRITE 1234 08:05:16911388 0 EOF"; a
			// waiter would have "->" before FLOCK.
			f := strings.Fields(line)
			if len(f) >= 7 && f[0] == "lock:" && f[2] == "FLOCK" && f[6] == id {
				return fd
			}
		}
	}
	return -1
}

// holders lists the pids /proc/locks records as holding an flock on the file
// id (see fileID). Waiters ("->" lines) are left out.
func holders(id string) []int {
	f, err := os.Open("/proc/locks")
	if err != nil {
		return nil
	}
	defer f.Close()
	var pids []int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// "1: FLOCK  ADVISORY  WRITE 1234 08:05:16911388 0 EOF"
		if len(fields) < 6 || fields[1] != "FLOCK" || fields[5] != id {
			continue
		}
		if pid, err := strconv.Atoi(fields[4]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ancestors returns the parent chain of pid, nearest first, ending before 0.
func ancestors(pid int) []int {
	var out []int
	for p := ppid(pid); p > 0 && len(out) < 4096; p = ppid(p) {
		out = append(out, p)
	}
	return out
}

func ppid(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// "pid (comm) state ppid ...": comm may hold spaces or parens.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	p, _ := strconv.Atoi(f[1])
	return p
}

func comm(pid int) string {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return strings.TrimSpace(string(b))
}

// major and minor decode a Linux dev_t (glibc's gnu_dev_major/minor).
func major(dev uint64) uint64 { return ((dev >> 8) & 0xfff) | ((dev >> 32) &^ 0xfff) }
func minor(dev uint64) uint64 { return (dev & 0xff) | ((dev >> 12) &^ 0xff) }
