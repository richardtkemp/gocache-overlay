// Command gocache-overlay is a GOCACHEPROG helper that reads an existing Go
// build cache and never writes to it. New build outputs go to a scratch
// directory that is thrown away, so mutation testing (thousands of builds
// whose outputs are never reused) gets cache hits for unchanged packages
// without growing GOCACHE.
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
	"strings"
	"syscall"
	"time"

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

	sharedDir, err := checkDirs(*shared, *root, os.Getenv("GOCACHE"))
	if err != nil {
		return fatal("%v", err)
	}
	sharedCache := native.Cache{Dir: sharedDir}
	if _, err := sharedCache.Probe(16); err != nil {
		return fatal("%v", err)
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
	if os.Getenv("GOCACHE_OVERLAY_DEBUG") != "" {
		srv.Debug = os.Stderr
	}
	if !ephemeral {
		srv.Touch = func() { _ = os.Chtimes(scratchDir, time.Now(), time.Now()) }
	}
	serveErr := srv.Serve(os.Stdin, os.Stdout)
	report(srv.Stats(), *statsFile, *verbose, sharedDir, scratchDir)
	if serveErr != nil {
		return fatal("%v", serveErr)
	}
	return 0
}

// checkDirs validates the shared dir and scratch root and returns the
// cleaned shared dir.
//
// GOCACHE must be a prefix of the shared dir: the go command copies files
// whose path starts with GOCACHE, but may chmod and rename any other file it
// installs from the cache (cmd/go/internal/work.moveOrCopyFile). A shared
// file outside GOCACHE could be moved out of the shared cache.
func checkDirs(shared, root, gocache string) (string, error) {
	if shared == "" {
		return "", errors.New("no shared cache dir: set GOCACHE or -shared")
	}
	if !filepath.IsAbs(shared) || !filepath.IsAbs(root) {
		return "", fmt.Errorf("shared dir %q and scratch root %q must be absolute", shared, root)
	}
	shared, root = filepath.Clean(shared), filepath.Clean(root)
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
	if within(root, shared) || within(shared, root) {
		return "", fmt.Errorf("scratch root %s and shared dir %s must not contain each other", root, shared)
	}
	return shared, nil
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func report(st overlay.Stats, statsFile string, verbose bool, shared, scratch string) {
	line := struct {
		overlay.Stats
		PID     int    `json:"pid"`
		Shared  string `json:"shared"`
		Scratch string `json:"scratch"`
		Time    string `json:"time"`
	}{st, os.Getpid(), shared, scratch, time.Now().UTC().Format(time.RFC3339)}
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
