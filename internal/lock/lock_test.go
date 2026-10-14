//go:build linux

package lock

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
