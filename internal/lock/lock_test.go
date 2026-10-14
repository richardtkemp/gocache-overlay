//go:build linux

package lock

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The lines the factory's lock-wait readers take (see main_test.go lockLines).
var (
	waitRE = regexp.MustCompile(`(?m)^>>> waiting for heavy lock[^\[]*\[\d\d:\d\d:\d\d\]`)
	gotRE  = regexp.MustCompile(`(?m)^>>> acquired heavy lock[^\[]*\[\d\d:\d\d:\d\d\]`)
)

func lockFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "heavy")
	if err := os.WriteFile(p, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	return p
}

// holdLock holds an flock on path from a non-ancestor process until release.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	cmd := exec.Command("flock", "-o", path, "-c", "echo locked; exec sleep 60")
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("flock: %v", err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("flock holder did not report the lock: %q", line)
	}
	var once sync.Once
	release = func() { once.Do(func() { cmd.Process.Kill(); cmd.Wait() }) }
	t.Cleanup(release)
	return release
}

// The waiting line is printed before blocking and the acquired line only once
// the lock is ours.
func TestWaitingLineBeforeBlockingAcquiredAfter(t *testing.T) {
	lk := lockFile(t)
	release := holdLock(t, lk)
	var log syncBuf
	l := &Lazy{Path: lk, Log: &log}
	done := make(chan error, 1)
	go func() { done <- l.Need() }()

	deadline := time.Now().Add(30 * time.Second) // hang guard only
	for !waitRE.MatchString(log.String()) {
		select {
		case err := <-done:
			t.Fatalf("Need returned (%v) while another process held the lock; log:\n%s", err, log.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no waiting line while blocked on the lock; log:\n%s", log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("Need returned (%v) while another process held the lock", err)
	default:
	}
	if gotRE.MatchString(log.String()) {
		t.Fatalf("acquired line printed while the lock was held elsewhere:\n%s", log.String())
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := log.String()
	if len(waitRE.FindAllString(out, -1)) != 1 || len(gotRE.FindAllString(out, -1)) != 1 ||
		waitRE.FindStringIndex(out)[0] > gotRE.FindStringIndex(out)[0] {
		t.Errorf("want one waiting line then one acquired line, got:\n%s", out)
	}
	if !strings.HasPrefix(l.State(), "taken after waiting ") {
		t.Errorf("state %q", l.State())
	}
}

// $HEAVY_LOCK_HELD: not taken, nothing printed.
func TestHeldByCallerIsSilent(t *testing.T) {
	lk := lockFile(t)
	holdLock(t, lk)
	t.Setenv(HeldEnv, lk)
	var log syncBuf
	l := &Lazy{Path: lk, Log: &log}
	if err := l.Need(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.State(), "held by caller") || log.String() != "" {
		t.Errorf("want held by caller and no lines, got state %q, log:\n%s", l.State(), log.String())
	}
}

// ---- a lock held through an ancestor's open file (#2418) ----

// subshellHolds is the build-script pattern of foci's Makefile: a subshell
// opens the lock file on fd 9, a flock(1) child takes the lock on it and
// exits, and the command runs with fd 9 closed. /proc/locks then names the
// dead flock child; only the subshell's fd (9, or where the shell moved it)
// holds the lock. A command after "$@" keeps the shell from exec'ing it (an
// exec with 9<&- would drop the lock).
const subshellHolds = `( flock 9; "$@" 9<&-; s=$?; exit $s ) 9<"$0"`

// TestHelperProcess is not a test: run as a child (by the tests below) it
// takes the lock in $LOCK_TEST_PATH with Lazy and prints its state.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("LOCK_TEST_HELPER") != "1" {
		t.Skip("helper process only")
	}
	var log syncBuf
	l := &Lazy{Path: os.Getenv("LOCK_TEST_PATH"), Log: &log}
	if err := l.Need(); err != nil {
		fmt.Printf("ERROR %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("STATE %s\nLOG %q\n", l.State(), log.String())
	os.Exit(0)
}

// groupCmd runs argv in its own process group, killed whole (with every
// descendant) when ctx ends, so a deadlocked child cannot outlive the test.
func groupCmd(ctx context.Context, env []string, argv ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(cleanEnv(), env...)
	return cmd
}

// cleanEnv is this process's environment without $HEAVY_LOCK_HELD.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, HeldEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// The lock held by an ancestor subshell's fd, taken there by a flock(1) child
// that has exited, is not waited for: waiting would deadlock, as the subshell
// waits for this process. Without the fdinfo check this test times out.
func TestLockHeldThroughAncestorFd(t *testing.T) {
	lk := lockFile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := groupCmd(ctx, []string{"LOCK_TEST_HELPER=1", "LOCK_TEST_PATH=" + lk},
		"sh", "-c", subshellHolds, lk, os.Args[0], "-test.run=^TestHelperProcess$")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("deadlock: the child waited for the lock its ancestor subshell holds\n%s", out)
	}
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m)^STATE held by ancestor \d+ through its fd \d+$`).Match(out) ||
		!strings.Contains(string(out), `LOG ""`) {
		t.Errorf("want held by the ancestor subshell's fd and no lock lines, got:\n%s", out)
	}
}

// The same shape held by a process that is not an ancestor is waited for:
// the fdinfo check looks at ancestors only.
func TestLockHeldThroughOtherProcessFdIsWaitedFor(t *testing.T) {
	lk := lockFile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder := groupCmd(ctx, nil, "sh", "-c", `( flock 9; echo locked; sleep 60; true ) 9<"$0"`, lk)
	stdout, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Wait()
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("holder did not report the lock: %q", line)
	}
	t.Setenv(HeldEnv, "")
	var log syncBuf
	l := &Lazy{Path: lk, Log: &log}
	done := make(chan error, 1)
	go func() { done <- l.Need() }()
	select {
	case err := <-done:
		t.Fatalf("Need returned (%v, state %q) while a non-ancestor held the lock", err, l.State())
	case <-time.After(500 * time.Millisecond):
	}
	cancel() // kills the holder's group, releasing the lock
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Need still blocked after the holder exited")
	}
	if !strings.HasPrefix(l.State(), "taken after waiting ") {
		t.Errorf("state %q", l.State())
	}
}
