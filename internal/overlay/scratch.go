package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Scratch directory naming under the scratch root:
//
//	run-<id>/          one per run id, shared by every go process carrying
//	                   that id; removed by RemoveRun (or the age sweep)
//	inv-<pid>-<rand>/  one per go invocation without an id; removed when
//	                   that invocation's helper exits (or by the age sweep)
const (
	runPrefix = "run-"
	invPrefix = "inv-"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateID reports whether id is usable as a run id (it becomes a path
// component, so no separators, no leading dot).
func ValidateID(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid run id %q: want 1-128 chars of [A-Za-z0-9._-], starting alphanumeric", id)
	}
	return nil
}

// RunDir returns the scratch dir for a run id.
func RunDir(root, id string) string { return filepath.Join(root, runPrefix+id) }

// OpenScratch creates (or reuses, for a run id) the scratch dir. ephemeral
// reports whether the caller owns it and must remove it at exit.
func OpenScratch(root, id string) (dir string, ephemeral bool, err error) {
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", false, err
	}
	if id != "" {
		if err := ValidateID(id); err != nil {
			return "", false, err
		}
		dir = RunDir(root, id)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return "", false, err
		}
		touch(dir)
		return dir, false, nil
	}
	dir, err = os.MkdirTemp(root, fmt.Sprintf("%s%d-", invPrefix, os.Getpid()))
	if err != nil {
		return "", false, err
	}
	return dir, true, nil
}

// touch marks dir as in use for the age sweep.
func touch(dir string) {
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
}

// Sweep removes scratch dirs under root whose own mtime is older than
// maxAge, except keep. It is the backstop for runs that never reached their
// cleanup step and helpers that were killed. maxAge <= 0 disables it.
// It returns the dirs removed.
func Sweep(root string, maxAge time.Duration, keep string) ([]string, error) {
	if maxAge <= 0 {
		return nil, nil
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	cutoff := time.Now().Add(-maxAge)
	var removed []string
	for _, de := range ents {
		name := de.Name()
		if !de.IsDir() || !(strings.HasPrefix(name, runPrefix) || strings.HasPrefix(name, invPrefix)) {
			continue
		}
		path := filepath.Join(root, name)
		if path == keep {
			continue
		}
		fi, err := de.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err == nil {
			removed = append(removed, path)
		}
	}
	return removed, nil
}

// RemoveRun deletes the scratch dir for id and returns the bytes it held.
// A missing dir is not an error (the sweep, or an earlier call, got there).
func RemoveRun(root, id string) (int64, error) {
	if err := ValidateID(id); err != nil {
		return 0, err
	}
	dir := RunDir(root, id)
	n, _ := DirBytes(dir)
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	return n, nil
}

// DirBytes sums the sizes of regular files under dir.
func DirBytes(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, err
}
