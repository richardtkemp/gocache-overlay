// Command gocache-overlay is a GOCACHEPROG helper that reads an existing Go
// build cache and never writes to it. New build outputs go to a scratch
// directory that is thrown away, so mutation testing (thousands of builds
// whose outputs are never reused) gets cache hits for unchanged packages
// without growing GOCACHE. -mode plain drops the overlay (it reads and writes
// GOCACHE like the go command), and -lock takes a host build lock only on the
// first cache miss.
//
// Usage: GOCACHEPROG=gocache-overlay go test ./...   (see README.md, or -h)
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/richardtkemp/gocache-overlay/internal/lock"
	"github.com/richardtkemp/gocache-overlay/internal/native"
	"github.com/richardtkemp/gocache-overlay/internal/overlay"
)

const usage = `gocache-overlay: a read-only overlay over a Go build cache, for GOCACHEPROG.

The go command starts it; you only set the environment:

    GOCACHEPROG=gocache-overlay go test ./...

Gets are served from the run's scratch dir, then from the shared cache
(default: $GOCACHE, which the go command always passes). Puts go only to the
scratch dir. The shared cache is never written.

Scratch dirs live under the scratch root:
  without a run id: <root>/inv-<pid>-*, removed when the go command exits
  with a run id:    <root>/run-<id>, shared by every go command carrying the
                    id, removed by 'gocache-overlay -remove-id <id>'
Every start also removes scratch dirs untouched for longer than -max-age.
The root may be a subdir of the shared dir (not a 00-ff entry dir), e.g.
$GOCACHE/overlay: go then copies the scratch files it installs, and whatever
lets go write GOCACHE (a sandbox) lets the helper write its scratch.

-mode plain reads AND writes the shared cache, as the go command does
without a helper (no scratch; hits refresh mtimes as go does): for -lock
without the overlay.

-lock <file> takes an flock on that file at the first cache miss, before
answering it, and holds it until exit: a go command the cache fully covers
never waits for a host-wide build lock. It does not wait when the process tree
already holds the lock: $HEAVY_LOCK_HELD names the file (a caller locked up
front), or /proc/locks shows an ancestor, or this program serving an ancestor
go command (a go build run by a test), holding it. The file is opened read-only
and never created; failing to open or lock it fails the build. When it takes
the lock it prints ">>> waiting for heavy lock ... [HH:MM:SS]" and
">>> acquired heavy lock [HH:MM:SS]" to stderr, which the go command passes
through as its own; it prints nothing when it does not take the lock.

GOCACHE_OVERLAY_DEBUG=1 logs every get and put to stderr.

Flags (each also settable by the environment variable shown):
`

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func defaultRoot() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("gocache-overlay-%d", os.Getuid()))
}

func main() {
	os.Exit(run())
}

func run() int {
	fl := flag.NewFlagSet("gocache-overlay", flag.ContinueOnError)
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), usage)
		fl.PrintDefaults()
	}
	shared := fl.String("shared", envOr("GOCACHE_OVERLAY_SHARED", os.Getenv("GOCACHE")),
		"shared native cache dir to read ($GOCACHE_OVERLAY_SHARED, else $GOCACHE)")
	root := fl.String("root", envOr("GOCACHE_OVERLAY_ROOT", defaultRoot()),
		"scratch root ($GOCACHE_OVERLAY_ROOT); must be outside the shared dir")
	id := fl.String("id", os.Getenv("GOCACHE_OVERLAY_ID"),
		"run id: share one scratch dir across go commands ($GOCACHE_OVERLAY_ID)")
	maxAgeDefault := 24 * time.Hour
	if v := os.Getenv("GOCACHE_OVERLAY_MAX_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gocache-overlay: GOCACHE_OVERLAY_MAX_AGE: %v\n", err)
			return 2
		}
		maxAgeDefault = d
	}
	maxAge := fl.Duration("max-age", maxAgeDefault,
		"remove scratch dirs untouched this long, at startup; 0 disables ($GOCACHE_OVERLAY_MAX_AGE)")
	statsFile := fl.String("stats", os.Getenv("GOCACHE_OVERLAY_STATS"),
		"append a JSON stats line to this file at exit ($GOCACHE_OVERLAY_STATS)")
	verbose := fl.Bool("v", os.Getenv("GOCACHE_OVERLAY_VERBOSE") != "",
		"print a stats line to stderr at exit ($GOCACHE_OVERLAY_VERBOSE)")
	removeID := fl.String("remove-id", "", "remove the scratch dir of this run id, then exit (no protocol)")
	mode := fl.String("mode", envOr("GOCACHE_OVERLAY_MODE", "overlay"),
		"overlay: read the shared cache, write scratch; plain: read and write the shared cache ($GOCACHE_OVERLAY_MODE)")
	lockPath := fl.String("lock", os.Getenv("GOCACHE_OVERLAY_LOCK"),
		"flock this file (opened read-only, never created) on the first cache miss and hold it until exit ($GOCACHE_OVERLAY_LOCK)")
	if err := fl.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	fatal := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "gocache-overlay: "+format+"\n", a...)
		return 1
	}

	if *removeID != "" {
		n, err := overlay.RemoveRun(*root, *removeID)
		if err != nil {
			return fatal("%v", err)
		}
		fmt.Printf("removed %s (%d bytes)\n", overlay.RunDir(*root, *removeID), n)
		return 0
	}

	if *mode != "overlay" && *mode != "plain" {
		return fatal("-mode %q: want overlay or plain", *mode)
	}
	plain := *mode == "plain"
	if *lockPath != "" && !filepath.IsAbs(*lockPath) {
		return fatal("-lock %q must be an absolute path", *lockPath)
	}
	var lazy *lock.Lazy
	if *lockPath != "" {
		lazy = &lock.Lazy{Path: *lockPath, Log: os.Stderr}
	}

	var sharedDir string
	var err error
	if plain {
		sharedDir, err = checkShared(*shared, os.Getenv("GOCACHE"))
	} else {
		sharedDir, err = checkDirs(*shared, *root, os.Getenv("GOCACHE"))
	}
	if err != nil {
		return fatal("%v", err)
	}
	sharedCache := native.Cache{Dir: sharedDir}
	if _, err := sharedCache.Probe(16); err != nil {
		return fatal("%v", err)
	}

	if plain {
		srv := &overlay.Server{
			Shared:  sharedCache,
			Scratch: native.Store{Cache: sharedCache, Perm: 0o666 &^ umask()},
		}
		return serve(srv, lazy, *statsFile, *verbose, sharedDir, "", fatal)
	}

	scratchDir, ephemeral, err := overlay.OpenScratch(*root, *id)
	if err != nil {
		return fatal("scratch: %v", err)
	}
	cleanup := func() {
		if ephemeral {
			os.RemoveAll(scratchDir)
		}
	}
	defer cleanup()
	if _, err := overlay.Sweep(*root, *maxAge, scratchDir); err != nil {
		fmt.Fprintf(os.Stderr, "gocache-overlay: sweep: %v\n", err)
	}

	// The go command and this helper share a process group, so ^C reaches
	// both. Clean up rather than leave the scratch dir to the sweep.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigs
		cleanup()
		os.Exit(1)
	}()

	srv := &overlay.Server{
		Shared:  sharedCache,
		Scratch: native.Store{Cache: native.Cache{Dir: scratchDir}},
	}
	if !ephemeral {
		srv.Touch = func() { _ = os.Chtimes(scratchDir, time.Now(), time.Now()) }
	}
	return serve(srv, lazy, *statsFile, *verbose, sharedDir, scratchDir, fatal)
}

// serve runs the protocol, with the lazy lock (if any) taken on the first
// miss, and writes the stats line.
func serve(srv *overlay.Server, lazy *lock.Lazy, statsFile string, verbose bool, shared, scratch string,
	fatal func(string, ...any) int) int {
	if os.Getenv("GOCACHE_OVERLAY_DEBUG") != "" {
		srv.Debug = os.Stderr
	}
	if lazy != nil {
		srv.OnMiss = lazy.Need
	}
	serveErr := srv.Serve(os.Stdin, os.Stdout)
	lockState := ""
	if lazy != nil {
		lockState = lazy.State()
		if lockState == "" {
			lockState = "not needed"
		}
	}
	report(srv.Stats(), statsFile, verbose, shared, scratch, lockState)
	if serveErr != nil {
		return fatal("%v", serveErr)
	}
	return 0
}

// umask returns the process umask (the go command's own files use 0666 less it).
func umask() os.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return os.FileMode(m)
}

// checkDirs validates the shared dir and scratch root and returns the
// cleaned shared dir.
//
// GOCACHE must be a prefix of the shared dir: the go command copies files
// whose path starts with GOCACHE, but may chmod and rename any other file it
// installs from the cache (cmd/go/internal/work.moveOrCopyFile). A shared
// file outside GOCACHE could be moved out of the shared cache.
func checkDirs(shared, root, gocache string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("scratch root %q must be absolute", root)
	}
	shared, err := checkShared(shared, gocache)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	if within(shared, root) {
		return "", fmt.Errorf("scratch root %s must not contain the shared dir %s", root, shared)
	}
	if within(root, shared) {
		// Inside the shared dir is allowed, beside the native entries: the go
		// command then copies (never renames) scratch files it installs, and a
		// seal that lets go write GOCACHE lets the helper write its scratch.
		// Not under a 00-ff entry dir: the shared reads, go's trim and
		// `go clean -cache` all own those.
		first, _, _ := strings.Cut(filepath.ToSlash(mustRel(shared, root)), "/")
		if nativeSubdir.MatchString(first) {
			return "", fmt.Errorf("scratch root %s must not be under one of the 00-ff entry dirs of the shared dir %s", root, shared)
		}
	}
	return shared, nil
}

// checkShared validates the shared dir (both modes) and returns it cleaned.
func checkShared(shared, gocache string) (string, error) {
	if shared == "" {
		return "", errors.New("no shared cache dir: set GOCACHE or -shared")
	}
	if !filepath.IsAbs(shared) {
		return "", fmt.Errorf("shared dir %q must be absolute", shared)
	}
	shared = filepath.Clean(shared)
	fi, err := os.Stat(shared)
	if err != nil {
		return "", fmt.Errorf("shared cache: %v", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("shared cache %s is not a directory", shared)
	}
	if gocache != "" && !strings.HasPrefix(shared, gocache) {
		return "", fmt.Errorf("GOCACHE=%s is not a prefix of the shared dir %s; the go command could move shared files away. Set GOCACHE to the shared dir", gocache, shared)
	}
	return shared, nil
}

// nativeSubdir names the native cache's entry dirs (<GOCACHE>/00 ... /ff).
var nativeSubdir = regexp.MustCompile(`^[0-9a-f]{2}$`)

func mustRel(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return path
	}
	return rel
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func report(st overlay.Stats, statsFile string, verbose bool, shared, scratch, lockState string) {
	line := struct {
		overlay.Stats
		PID     int    `json:"pid"`
		Shared  string `json:"shared"`
		Scratch string `json:"scratch,omitempty"`
		Lock    string `json:"lock,omitempty"`
		Time    string `json:"time"`
	}{st, os.Getpid(), shared, scratch, lockState, time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(line)
	if verbose {
		fmt.Fprintf(os.Stderr, "gocache-overlay: %s\n", b)
	}
	if statsFile != "" {
		f, err := os.OpenFile(statsFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
}
