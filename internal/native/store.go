package native

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Store is a writable cache directory in the native layout. Every file is
// written to a temporary name in its final directory and renamed into
// place, so concurrent writers (several go processes sharing one run id)
// and concurrent readers only ever see absent or complete files.
type Store struct {
	Cache
}

// Put stores body as the output for actionID and returns the body's path.
// outputID must be the SHA-256 of body (the go command computes it).
func (s Store) Put(actionID, outputID, body []byte, now time.Time) (string, error) {
	if len(actionID) != HashSize || len(outputID) != HashSize {
		return "", fmt.Errorf("put: bad ID length (action %d, output %d)", len(actionID), len(outputID))
	}
	dpath := s.fileName(outputID, "d")
	// Bodies are content-addressed: if a complete one is already there
	// (another writer, or an earlier put), keep it.
	if fi, err := os.Stat(dpath); err != nil || !fi.Mode().IsRegular() || fi.Size() != int64(len(body)) {
		if err := atomicWrite(dpath, body); err != nil {
			return "", err
		}
	}
	line := fmt.Sprintf("v1 %x %x %20d %20d\n", actionID, outputID, len(body), now.UnixNano())
	if err := atomicWrite(s.fileName(actionID, "a"), []byte(line)); err != nil {
		return "", err
	}
	return dpath, nil
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// CreateTemp makes 0600 files; make them readable like the native
		// cache's (0666 before umask).
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
