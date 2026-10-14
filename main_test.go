package main

// Integration tests: real go commands with GOCACHEPROG pointing at a freshly
// built helper, against a throwaway shared cache seeded natively by the
// installed Go toolchain. Because the seed is written by the real go
// command, these tests are also the format check for a Go upgrade: if the
// native layout changes, TestUnchangedBuildIsAllSharedHits and
// TestEveryNativeEntryParses fail instead of the helper silently missing.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richardtkemp/gocache-overlay/internal/lock"
	"github.com/richardtkemp/gocache-overlay/internal/native"
	"github.com/richardtkemp/gocache-overlay/internal/overlay"
	"github.com/richardtkemp/gocache-overlay/internal/treehash"
)

var (
	helperBin string // built helper
	seedCache string // shared cache, seeded natively; tests must never change it
	modSrc    string // pristine copy of the test module
)

const libSrc = `package lib

import (
	"fmt"
	"strings"
)

func Hello(name string) string { return fmt.Sprintf("hello, %s", strings.ToUpper(name)) }
`

const libTest = `package lib

import "testing"

func TestHello(t *testing.T) {
	if Hello("x") == "" {
		t.Fatal("empty")
	}
}
`

const appSrc = `package main

import (
	"fmt"

	"example.com/m/lib"
)

func main() { fmt.Println(lib.Hello("world")) }
`

func TestMain(m *testing.M) {
	code, err := setup(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		code = 1
	}
	os.Exit(code)
}

func setup(m *testing.M) (int, error) {
	tmp, err := os.MkdirTemp("", "gocache-overlay-test-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(tmp)
	helperBin = filepath.Join(tmp, "bin", "gocache-overlay")
	if out, err := exec.Command("go", "build", "-o", helperBin, ".").CombinedOutput(); err != nil {
		return 1, fmt.Errorf("building helper: %v\n%s", err, out)
	}
	modSrc = filepath.Join(tmp, "src")
	writeModule(modSrc)
	backdate(modSrc)
	seedCache = filepath.Join(tmp, "shared")
	// Seed natively: no GOCACHEPROG.
	for _, args := range [][]string{{"build", "./..."}, {"test", "./lib"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = modSrc
		cmd.Env = goEnv(seedCache, "")
		if out, err := cmd.CombinedOutput(); err != nil {
			return 1, fmt.Errorf("seeding %v: %v\n%s", args, err, out)
		}
	}
	return m.Run(), nil
}

func writeModule(dir string) {
	files := map[string]string{
		"go.mod":          "module example.com/m\n\ngo 1.24\n",
		"lib/lib.go":      libSrc,
		"lib/lib_test.go": libTest,
		"cmd/app/main.go": appSrc,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o777)
		os.WriteFile(p, []byte(body), 0o644)
	}
}

// backdate sets every file in dir an hour old. The go command declines to
// cache some results that depend on files modified in the last couple of
// seconds, which would make hit counts depend on test timing.
func backdate(dir string) {
	old := time.Now().Add(-time.Hour)
	filepath.WalkDir(dir, func(p string, _ fs.DirEntry, _ error) error {
		os.Chtimes(p, old, old)
		return nil
	})
}

// goEnv is a clean environment for a child go command.
func goEnv(gocache, gocacheprog string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "GOCACHE", k == "GOCACHEPROG", k == "GOFLAGS", k == "GOWORK", k == "GOTOOLCHAIN",
			k == "CGO_ENABLED", strings.HasPrefix(k, "GOCACHE_OVERLAY_"), strings.HasPrefix(k, "GODEBUG"):
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GOCACHE="+gocache, "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if gocacheprog != "" {
		env = append(env, "GOCACHEPROG="+gocacheprog)
	}
	return env
}

// module copies the test module to a temp dir, applying a mutation (a
// string appended to lib.go) if non-empty.
func module(t *testing.T, mutation string) string {
	t.Helper()
	dir := t.TempDir()
	writeModule(dir)
	if mutation != "" {
		f, _ := os.OpenFile(filepath.Join(dir, "lib", "lib.go"), os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("\n" + mutation + "\n")
		f.Close()
	}
	backdate(dir)
	return dir
}

type runResult struct {
	stats  overlay.Stats
	output string
	err    error
	lock   []string // each helper process's lock state, in stats-line order
}

func readLockStates(statsFile string) []string {
	data, _ := os.ReadFile(statsFile)
	var states []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var line struct {
			Lock string `json:"lock"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil {
			states = append(states, line.Lock)
		}
	}
	return states
}

// goOverlay runs a go command with the helper over shared.
func goOverlay(t *testing.T, dir, shared, root string, helperArgs []string, args ...string) runResult {
	t.Helper()
	return goOverlayEnv(t, dir, shared, root, helperArgs, nil, args...)
}

// goOverlayEnv is goOverlay with extra environment for the go command.
func goOverlayEnv(t *testing.T, dir, shared, root string, helperArgs, extraEnv []string, args ...string) runResult {
	t.Helper()
	return goOverlayWrapped(t, nil, dir, shared, root, helperArgs, extraEnv, args...)
}

// goOverlayWrapped is goOverlayEnv with the go command run under wrap (a
// command prefix such as flock FILE), or directly when wrap is empty.
func goOverlayWrapped(t *testing.T, wrap []string, dir, shared, root string, helperArgs, extraEnv []string, args ...string) runResult {
	t.Helper()
	statsFile := filepath.Join(t.TempDir(), "stats.jsonl")
	prog := strings.Join(append([]string{helperBin, "-root", root, "-stats", statsFile}, helperArgs...), " ")
	argv := append(append(append([]string{}, wrap...), "go"), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(goEnv(shared, prog), extraEnv...)
	out, err := cmd.CombinedOutput()
	r := runResult{output: string(out), err: err}
	r.lock = readLockStates(statsFile)
	data, _ := os.ReadFile(statsFile)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() { // one line per helper process; sum them
		var st overlay.Stats
		if json.Unmarshal(sc.Bytes(), &st) == nil {
			r.stats.Gets += st.Gets
			r.stats.ScratchHits += st.ScratchHits
			r.stats.SharedHits += st.SharedHits
			r.stats.Misses += st.Misses
			r.stats.Puts += st.Puts
			r.stats.PutBytes += st.PutBytes
		}
	}
	return r
}

func mustOK(t *testing.T, r runResult) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("go command failed: %v\n%s", r.err, r.output)
	}
}

func fingerprint(t *testing.T, dir string, skip ...string) treehash.Sum {
	t.Helper()
	s, err := treehash.Of(dir, skip...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func scratchDirs(t *testing.T, root string) []string {
	t.Helper()
	ents, _ := os.ReadDir(root)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// Requirement 1 and 5: an unchanged build is served entirely from the
// shared cache. A format change in a new Go release turns this red (or the
// helper exits on its format probe), never a silent all-miss.
func TestUnchangedBuildIsAllSharedHits(t *testing.T) {
	root := t.TempDir()
	// ./lib only: a main package is relinked on every go build, cached or
	// not, and that link action always misses (natively too).
	// In the seed's own dir: action IDs include the package directory, so a
	// copy elsewhere would (correctly) miss on its own packages.
	r := goOverlay(t, modSrc, seedCache, root, nil, "build", "./lib")
	mustOK(t, r)
	t.Logf("stats: %+v", r.stats)
	if r.stats.SharedHits == 0 || r.stats.Misses != 0 || r.stats.Puts != 0 {
		t.Fatalf("unchanged build: want only shared hits, got %+v", r.stats)
	}
}

// Requirement 5: every entry the installed go command wrote parses.
func TestEveryNativeEntryParses(t *testing.T) {
	n := 0
	err := filepath.WalkDir(seedCache, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "-a") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := native.ParseEntry(data, nil); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("native entry written by %s does not parse: %v", goVersion(t), err)
	}
	if n < 10 {
		t.Fatalf("only %d action entries in the seeded cache: has the layout changed?", n)
	}
	t.Logf("%d native entries parsed (%s)", n, goVersion(t))
}

func goVersion(t *testing.T) string {
	out, _ := exec.Command("go", "version").Output()
	return strings.TrimSpace(string(out))
}

// Requirements 1, 2 and 4: a mutated build hits the shared cache for
// unchanged packages, writes its new outputs only to scratch, leaves the
// shared dir byte-for-byte unchanged, and removes its scratch dir at exit.
func TestMutantBuildLeavesSharedUnchanged(t *testing.T) {
	before := fingerprint(t, seedCache)
	root := t.TempDir()
	r := goOverlay(t, module(t, "func Mutant() int { return 42 }"), seedCache, root, nil, "test", "./...")
	mustOK(t, r)
	t.Logf("stats: %+v", r.stats)
	if r.stats.SharedHits == 0 {
		t.Errorf("no shared hits: %+v", r.stats)
	}
	if r.stats.Puts == 0 || r.stats.PutBytes == 0 {
		t.Errorf("mutant produced no new outputs, test proves nothing: %+v", r.stats)
	}
	if after := fingerprint(t, seedCache); after != before {
		t.Errorf("shared cache changed:\nbefore %v\nafter  %v", before, after)
	}
	if left := scratchDirs(t, root); len(left) != 0 {
		t.Errorf("scratch not removed at exit: %v", left)
	}
}

// Requirement 7: with a run id, a second go command in the same run reads
// the first one's outputs; -remove-id deletes the run's dir.
func TestRunIDSharedAcrossInvocations(t *testing.T) {
	before := fingerprint(t, seedCache)
	root := t.TempDir()
	dir := module(t, "func Mutant() int { return 7 }")
	id := []string{"-id", "wf-123"}
	r1 := goOverlay(t, dir, seedCache, root, id, "test", "./...")
	mustOK(t, r1)
	r2 := goOverlay(t, dir, seedCache, root, id, "test", "./...")
	mustOK(t, r2)
	t.Logf("first: %+v\nsecond: %+v", r1.stats, r2.stats)
	if r1.stats.Puts == 0 {
		t.Fatalf("first run put nothing: %+v", r1.stats)
	}
	if r2.stats.ScratchHits == 0 || r2.stats.Misses != 0 || r2.stats.Puts != 0 {
		t.Fatalf("second run should be served from the run dir + shared: %+v", r2.stats)
	}
	if !strings.Contains(r2.output, "(cached)") {
		t.Errorf("test result not cached across the run:\n%s", r2.output)
	}
	runDir := overlay.RunDir(root, "wf-123")
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("run dir missing between invocations: %v", err)
	}
	out, err := exec.Command(helperBin, "-root", root, "-remove-id", "wf-123").CombinedOutput()
	if err != nil {
		t.Fatalf("-remove-id: %v\n%s", err, out)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("run dir still present after -remove-id: %v", err)
	}
	if after := fingerprint(t, seedCache); after != before {
		t.Errorf("shared cache changed:\nbefore %v\nafter  %v", before, after)
	}
}

// Requirement 9: several go processes write to one run dir at once. All
// succeed, and afterwards every one of their builds is a full hit (no torn
// or missing entries).
func TestConcurrentGoProcessesOneRunID(t *testing.T) {
	before := fingerprint(t, seedCache)
	root := t.TempDir()
	id := []string{"-id", "concurrent"}
	const n = 4
	// Two mutants, each built by two processes at once in the same dir, so
	// writers race on identical keys as well as on the shared run dir.
	// -count=1: the go command does not cache test results reliably across
	// concurrent runs even natively, so assert on the build cache only.
	mutants := []string{module(t, "func Mutant() int { return 0 }"), module(t, "func Mutant() int { return 1 }")}
	dirs := make([]string, n)
	for i := range dirs {
		dirs[i] = mutants[i%2]
	}
	var wg sync.WaitGroup
	results := make([]runResult, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = goOverlay(t, dirs[i], seedCache, root, id, "test", "-count=1", "./...")
		}()
	}
	wg.Wait()
	for i, r := range results {
		mustOK(t, r)
		t.Logf("concurrent %d: %+v", i, r.stats)
	}
	if tmp, _ := filepath.Glob(filepath.Join(overlay.RunDir(root, "concurrent"), "*", ".tmp-*")); len(tmp) != 0 {
		t.Errorf("temp files left in run dir: %v", tmp)
	}
	// Replay each build twice. The second replay is the steady state (the
	// go command always relinks a test binary, which is one inherent miss
	// with a 0-byte put); the first must already match it, i.e. the
	// concurrent writers left nothing torn or missing.
	for i := range n {
		r1 := goOverlay(t, dirs[i], seedCache, root, id, "test", "-count=1", "./...")
		mustOK(t, r1)
		r2 := goOverlay(t, dirs[i], seedCache, root, id, "test", "-count=1", "./...")
		mustOK(t, r2)
		if r1.stats != r2.stats || r1.stats.PutBytes != 0 || r1.stats.ScratchHits == 0 {
			t.Errorf("replay %d after concurrent writers: %+v, steady state %+v", i, r1.stats, r2.stats)
		}
	}
	if after := fingerprint(t, seedCache); after != before {
		t.Errorf("shared cache changed:\nbefore %v\nafter  %v", before, after)
	}
}

// Requirement 8: a start sweeps scratch dirs untouched for longer than
// -max-age, and keeps fresh ones.
func TestStartupSweep(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "run-crashed")
	fresh := filepath.Join(root, "run-live")
	for _, d := range []string{stale, fresh} {
		os.MkdirAll(filepath.Join(d, "00"), 0o777)
	}
	old := time.Now().Add(-3 * time.Hour)
	os.Chtimes(stale, old, old)
	mustOK(t, goOverlay(t, module(t, ""), seedCache, root, []string{"-max-age", "2h"}, "build", "./..."))
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale run dir not swept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh run dir swept: %v", err)
	}
}

// Requirement 5, runtime arm: a shared cache in an unknown format stops the
// build with a clear message instead of degrading to misses.
func TestUnknownFormatFailsLoudly(t *testing.T) {
	shared := t.TempDir()
	p := filepath.Join(shared, "ab", strings.Repeat("ab", 32)+"-a")
	os.MkdirAll(filepath.Dir(p), 0o777)
	os.WriteFile(p, []byte("v2 some future layout\n"), 0o666)
	r := goOverlay(t, module(t, ""), shared, t.TempDir(), nil, "build", "./...")
	if r.err == nil || !strings.Contains(r.output, "format not recognised") {
		t.Fatalf("want a failed build naming the format problem, got err=%v\n%s", r.err, r.output)
	}
}

// GOCACHE must cover the shared dir, or the go command may rename shared
// files when installing them.
func TestGOCACHEMustCoverShared(t *testing.T) {
	other := t.TempDir()
	statsFile := filepath.Join(t.TempDir(), "s")
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = module(t, "")
	cmd.Env = goEnv(other, strings.Join([]string{helperBin, "-root", t.TempDir(), "-shared", seedCache, "-stats", statsFile}, " "))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "is not a prefix of the shared dir") {
		t.Fatalf("want refusal, got err=%v\n%s", err, out)
	}
}

// A scratch root inside the shared dir, beside the 00-ff entry dirs (the
// factory's <GOCACHE>/overlay): a run's outputs persist across go commands
// there, the shared entries stay byte-for-byte unchanged, and -remove-id
// deletes the run dir. A root that is the shared dir or lies under an entry
// dir is refused.
func TestRootInsideShared(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared")
	if out, err := exec.Command("cp", "-a", seedCache, shared).CombinedOutput(); err != nil {
		t.Fatalf("copying the seeded cache: %v\n%s", err, out)
	}
	// Created first: making it changes the shared dir's own mtime, once.
	root := filepath.Join(shared, "overlay")
	if err := os.Mkdir(root, 0o777); err != nil {
		t.Fatal(err)
	}
	before := fingerprint(t, shared, "overlay")
	dir := module(t, "func Mutant() int { return 11 }")
	id := []string{"-id", "run-inside"}
	r1 := goOverlay(t, dir, shared, root, id, "test", "./...")
	mustOK(t, r1)
	r2 := goOverlay(t, dir, shared, root, id, "test", "./...")
	mustOK(t, r2)
	t.Logf("first: %+v\nsecond: %+v", r1.stats, r2.stats)
	if r1.stats.SharedHits == 0 || r1.stats.Puts == 0 {
		t.Fatalf("first run: want shared hits and puts, got %+v", r1.stats)
	}
	if r2.stats.ScratchHits == 0 || r2.stats.Puts != 0 {
		t.Fatalf("second run should be served from the run dir + shared: %+v", r2.stats)
	}
	runDir := overlay.RunDir(root, "run-inside")
	if !strings.HasPrefix(runDir, shared+"/overlay/") {
		t.Fatalf("run dir %s is not under %s/overlay", runDir, shared)
	}
	if after := fingerprint(t, shared, "overlay"); after != before {
		t.Errorf("shared entries changed:\nbefore %v\nafter  %v", before, after)
	}
	if out, err := exec.Command(helperBin, "-root", root, "-remove-id", "run-inside").CombinedOutput(); err != nil {
		t.Fatalf("-remove-id: %v\n%s", err, out)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("run dir still present after -remove-id: %v", err)
	}

	for _, bad := range []string{filepath.Join(shared, "ab"), filepath.Join(shared, "ab", "x")} {
		r := goOverlay(t, module(t, ""), shared, bad, nil, "build", "./...")
		if r.err == nil || !strings.Contains(r.output, "must not be under one of the 00-ff entry dirs") {
			t.Errorf("root %s: want refusal, got err=%v\n%s", bad, r.err, r.output)
		}
	}
	for _, bad := range []string{shared, filepath.Dir(shared)} {
		r := goOverlay(t, module(t, ""), shared, bad, nil, "build", "./...")
		if r.err == nil || !strings.Contains(r.output, "must not contain the shared dir") {
			t.Errorf("root %s: want refusal, got err=%v\n%s", bad, r.err, r.output)
		}
	}
}

// ---- lazy lock (-lock) and plain mode (-mode plain), #2396 ----

// holdLock holds an flock on path from a separate process (not an ancestor of
// the go commands the test runs) until the test ends or release is called.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	cmd := exec.Command("flock", "-o", path, "-c", "echo locked; exec sleep 60") // -o: only flock itself holds it
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("flock: %v", err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if line != "locked\n" {
		t.Fatalf("flock holder did not report the lock: %q", line)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			cmd.Process.Kill()
			cmd.Wait()
		})
	}
	t.Cleanup(release)
	return release
}

// The factory's lock-wait readers, as they match a build's output lines: a
// line starting ">>>" (foci-factory scripts/run-timeline.py) and the regexes
// of temporal-factory factory/steps.py, which need a [HH:MM:SS] stamp.
var (
	waitLineRE = regexp.MustCompile(`(?i)waiting for (?:the )?(?:remote )?heavy(?: build)? lock[^\[]*\[(\d\d:\d\d:\d\d)\]`)
	gotLineRE  = regexp.MustCompile(`(?i)acquired (?:the )?(?:remote )?heavy(?: build)? lock[^\[]*\[(\d\d:\d\d:\d\d)\]`)
)

// lockLines returns, in order, "w" for each waiting line and "g" for each
// acquired line in a go command's output that both readers would take.
func lockLines(output string) string {
	var seq string
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), ">>>") {
			continue
		}
		switch {
		case waitLineRE.MatchString(line):
			seq += "w"
		case gotLineRE.MatchString(line):
			seq += "g"
		}
	}
	return seq
}

// wantNoLockLines fails if the helper printed lock lines although it did not take the lock.
func wantNoLockLines(t *testing.T, r runResult) {
	t.Helper()
	if seq := lockLines(r.output); seq != "" || strings.Contains(r.output, "heavy lock") {
		t.Errorf("lock not taken, but lock lines (%q) were printed:\n%s", seq, r.output)
	}
}

func lockFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "heavy")
	if err := os.WriteFile(p, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	return p
}

// plainCache is a private copy of the seeded cache, for plain mode (which writes it).
func plainCache(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shared")
	if out, err := exec.Command("cp", "-a", seedCache, dir).CombinedOutput(); err != nil {
		t.Fatalf("copying the seeded cache: %v\n%s", err, out)
	}
	return dir
}

// A go command the cache fully covers never takes the lock, so it runs while
// another process holds it.
func TestLockNotNeededWhenFullyCached(t *testing.T) {
	lk := lockFile(t)
	holdLock(t, lk)
	for _, mode := range []string{"overlay", "plain"} {
		r := goOverlay(t, modSrc, seedCache, t.TempDir(), []string{"-mode", mode, "-lock", lk}, "build", "./lib")
		mustOK(t, r)
		if r.stats.Misses != 0 || len(r.lock) != 1 || r.lock[0] != "not needed" {
			t.Errorf("%s: fully cached build: want no miss and lock %q, got %+v lock %q", mode, "not needed", r.stats, r.lock)
		}
		wantNoLockLines(t, r)
	}
}

// The first miss waits for the lock and holds it: the build does not finish
// while another process holds the lock, and finishes once it is released.
func TestLockTakenOnFirstMiss(t *testing.T) {
	lk := lockFile(t)
	release := holdLock(t, lk)
	done := make(chan runResult, 1)
	dir := module(t, "func Mutant() int { return 21 }")
	go func() {
		done <- goOverlay(t, dir, seedCache, t.TempDir(), []string{"-lock", lk}, "build", "./...")
	}()
	select {
	case r := <-done:
		t.Fatalf("build with a miss finished while the lock was held elsewhere: %+v lock %q\n%s", r.stats, r.lock, r.output)
	case <-time.After(3 * time.Second):
	}
	release()
	r := <-done
	mustOK(t, r)
	if r.stats.Misses == 0 || len(r.lock) != 1 || !strings.HasPrefix(r.lock[0], "taken after waiting ") {
		t.Errorf("want a miss and the lock taken after waiting, got %+v lock %q", r.stats, r.lock)
	}
	// The go command passes the helper's stderr through as its own, so the
	// build's output carries the pair the factory's lock-wait charts read.
	if seq := lockLines(r.output); seq != "wg" {
		t.Errorf("want one waiting line then one acquired line in the build's output, got %q:\n%s", seq, r.output)
	}
}

// An uncontended take still prints the pair (a zero-length wait), as a
// script's echo-flock-echo does.
func TestLockTakenUncontendedPrintsLines(t *testing.T) {
	lk := lockFile(t)
	for _, mode := range []string{"overlay", "plain"} {
		shared := seedCache
		if mode == "plain" {
			shared = plainCache(t)
		}
		r := goOverlay(t, module(t, "func Mutant() int { return 26 }"), shared, t.TempDir(),
			[]string{"-mode", mode, "-lock", lk}, "build", "./...")
		mustOK(t, r)
		if len(r.lock) != 1 || r.lock[0] != "taken" {
			t.Errorf("%s: want the lock taken, got lock %q", mode, r.lock)
		}
		if seq := lockLines(r.output); seq != "wg" {
			t.Errorf("%s: want one waiting line then one acquired line, got %q:\n%s", mode, seq, r.output)
		}
	}
}

// The lock held by an ancestor (in /proc/locks) is not waited for, and no
// lines are printed.
func TestLockHeldByAncestor(t *testing.T) {
	lk := lockFile(t)
	r := goOverlayWrapped(t, []string{"flock", lk}, module(t, "func Mutant() int { return 27 }"), seedCache, t.TempDir(),
		[]string{"-lock", lk}, nil, "build", "./...")
	mustOK(t, r)
	if r.stats.Misses == 0 || len(r.lock) != 1 || !strings.HasPrefix(r.lock[0], "held by ancestor ") {
		t.Errorf("want a miss and the lock held by an ancestor, got %+v lock %q", r.stats, r.lock)
	}
	wantNoLockLines(t, r)
}

// A caller that holds the lock up front says so in $HEAVY_LOCK_HELD; the
// helper then does not wait for it (it would wait on its own caller).
func TestLockHeldByCaller(t *testing.T) {
	lk := lockFile(t)
	holdLock(t, lk)
	r := goOverlayEnv(t, module(t, "func Mutant() int { return 22 }"), seedCache, t.TempDir(), []string{"-lock", lk},
		[]string{lock.HeldEnv + "=/other:" + lk}, "build", "./...")
	mustOK(t, r)
	if r.stats.Misses == 0 || len(r.lock) != 1 || r.lock[0] != "held by caller ($HEAVY_LOCK_HELD)" {
		t.Errorf("want a miss and the lock held by the caller, got %+v lock %q", r.stats, r.lock)
	}
	wantNoLockLines(t, r)
}

// The lock file is opened read-only and never created; a missing one fails
// the build instead of running unlocked.
func TestLockFileMissingFailsTheBuild(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-lock")
	r := goOverlay(t, module(t, "func Mutant() int { return 23 }"), seedCache, t.TempDir(), []string{"-lock", missing},
		"build", "./...")
	if r.err == nil || !strings.Contains(r.output, "lock: open "+missing) {
		t.Fatalf("want a failed build naming the lock file, got err=%v\n%s", r.err, r.output)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the lock file was created: %v", err)
	}
}

const nestTest = `package nest

import (
	"os"
	"os/exec"
	"testing"
)

// A test that runs a go build, as foci's tests do (testharness, cmd/foci).
func TestNestedBuild(t *testing.T) {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = os.Getenv("NEST_DIR")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nested go build: %v\n%s", err, out)
	}
	t.Logf("nested go build output:\n%s", out) // shown under -v: the nested helper's lock lines, if any
}
`

// A go command run by a test, while the test's own go command's helper holds
// the lock, must not wait for it: the holder waits for the test.
func TestNestedGoBuildDoesNotDeadlock(t *testing.T) {
	// Under another name, as an install may be: the helpers recognise each other by their own name.
	renamed := filepath.Join(t.TempDir(), "gco")
	if out, err := exec.Command("cp", helperBin, renamed).CombinedOutput(); err != nil {
		t.Fatalf("copy helper: %v\n%s", err, out)
	}
	defer func(b string) { helperBin = b }(helperBin)
	helperBin = renamed
	lk := lockFile(t)
	nest := t.TempDir()
	os.WriteFile(filepath.Join(nest, "go.mod"), []byte("module example.com/nest\n\ngo 1.24\n"), 0o644)
	os.WriteFile(filepath.Join(nest, "nest_test.go"), []byte(nestTest), 0o644)
	inner := module(t, "func Mutant() int { return 24 }") // misses: the nested helper needs the lock too
	r := goOverlayEnv(t, nest, seedCache, t.TempDir(), []string{"-lock", lk}, []string{"NEST_DIR=" + inner},
		"test", "-v", "-count=1", "-timeout=60s", "./...")
	mustOK(t, r)
	t.Logf("lock states: %q", r.lock)
	if !strings.Contains(r.output, "nested go build output:") {
		t.Fatalf("the nested build's output is not in the test output:\n%s", r.output)
	}
	// Only the outer helper, which took the lock, prints the pair.
	if seq := lockLines(r.output); seq != "wg" {
		t.Errorf("want only the outer helper's waiting and acquired lines, got %q:\n%s", seq, r.output)
	}
	var outer, nested int
	for _, st := range r.lock {
		switch {
		case st == "taken":
			outer++
		case strings.HasPrefix(st, "held by ") && strings.Contains(st, ", the helper of ancestor "):
			nested++
		}
	}
	if outer != 1 || nested != 1 {
		t.Errorf("want the outer helper to take the lock and the nested one to see it held by it, got %q", r.lock)
	}
}

// Plain mode reads and writes the shared cache like the go command itself: a
// mutant's outputs land in the shared dir, a second build hits them there, no
// scratch dir is made, and a hit on an entry older than an hour refreshes its
// mtimes (so mtime-based trimming sees it as in use).
func TestPlainModeWritesTheSharedCache(t *testing.T) {
	shared := plainCache(t)
	root := t.TempDir()
	dir := module(t, "func Mutant() int { return 25 }")
	plain := []string{"-mode", "plain"}
	r1 := goOverlay(t, dir, shared, root, plain, "build", "./lib")
	mustOK(t, r1)
	if r1.stats.Puts == 0 || r1.stats.ScratchHits != 0 {
		t.Fatalf("first plain build: want puts, got %+v", r1.stats)
	}
	if left := scratchDirs(t, root); len(left) != 0 {
		t.Errorf("plain mode made scratch dirs: %v", left)
	}
	// Age every file, then hit them again.
	old := time.Now().Add(-2 * time.Hour)
	filepath.WalkDir(shared, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			os.Chtimes(p, old, old)
		}
		return nil
	})
	r2 := goOverlay(t, dir, shared, root, plain, "build", "./lib")
	mustOK(t, r2)
	if r2.stats.Misses != 0 || r2.stats.Puts != 0 || r2.stats.SharedHits == 0 {
		t.Fatalf("second plain build: want only shared hits, got %+v", r2.stats)
	}
	fresh := 0
	filepath.WalkDir(shared, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) < time.Hour {
				fresh++
			}
		}
		return nil
	})
	if fresh < int(r2.stats.SharedHits) {
		t.Errorf("hits did not refresh mtimes: %d fresh files for %d hits", fresh, r2.stats.SharedHits)
	}
	// A native go command reads what plain mode wrote.
	cmd := exec.Command("go", "build", "./lib")
	cmd.Dir = dir
	cmd.Env = goEnv(shared, "")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native build over the plain-mode cache: %v\n%s", err, out)
	}
}

func TestModeMustBeKnown(t *testing.T) {
	r := goOverlay(t, module(t, ""), seedCache, t.TempDir(), []string{"-mode", "weird"}, "build", "./...")
	if r.err == nil || !strings.Contains(r.output, `-mode "weird": want overlay or plain`) {
		t.Fatalf("want refusal, got err=%v\n%s", r.err, r.output)
	}
}
