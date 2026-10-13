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
	"strings"
	"sync"
	"testing"
	"time"

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
}

// goOverlay runs a go command with the helper over shared.
func goOverlay(t *testing.T, dir, shared, root string, helperArgs []string, args ...string) runResult {
	t.Helper()
	statsFile := filepath.Join(t.TempDir(), "stats.jsonl")
	prog := strings.Join(append([]string{helperBin, "-root", root, "-stats", statsFile}, helperArgs...), " ")
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = goEnv(shared, prog)
	out, err := cmd.CombinedOutput()
	r := runResult{output: string(out), err: err}
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

func fingerprint(t *testing.T, dir string) treehash.Sum {
	t.Helper()
	s, err := treehash.Of(dir)
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
