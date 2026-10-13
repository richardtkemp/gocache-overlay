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
//   - the holder is another process of this program (same /proc/<pid>/comm)
//     whose parent is an ancestor of this process: a go command run by a test that its own go command's
//     helper is holding the lock for (a nested `go build` in a test).
package lock

import (
	"bufio"
	"errors"
	"fmt"
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
		return "taken", nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return "", fmt.Errorf("lock %s: %w", l.Path, err)
	}
	if who := heldByLineage(f); who != "" {
		f.Close()
		return who, nil
	}
	start := time.Now()
	if err := flockRetry(fd); err != nil {
		f.Close()
		return "", fmt.Errorf("lock %s: %w", l.Path, err)
	}
	l.f = f
	return fmt.Sprintf("taken after waiting %s", time.Since(start).Round(time.Millisecond)), nil
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
	for _, h := range holders(st.Dev, st.Ino) {
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
	return ""
}

// holders lists the pids /proc/locks records as holding an flock on the file
// (dev, ino). Waiters ("->" lines) are left out.
func holders(dev, ino uint64) []int {
	f, err := os.Open("/proc/locks")
	if err != nil {
		return nil
	}
	defer f.Close()
	// The kernel prints the device as %02x:%02x (major:minor) and the inode in
	// decimal.
	want := fmt.Sprintf("%02x:%02x:%d", major(dev), minor(dev), ino)
	var pids []int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// "1: FLOCK  ADVISORY  WRITE 1234 08:05:16911388 0 EOF"
		if len(fields) < 6 || fields[1] != "FLOCK" || fields[5] != want {
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
