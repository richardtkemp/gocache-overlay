// Package treehash fingerprints a directory tree for "unchanged" checks:
// every entry's relative path, type, mode, size, mtime and (for regular
// files) SHA-256 of its contents.
package treehash

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Sum is a fingerprint plus counts, for readable failures.
type Sum struct {
	Hash  string
	Files int
	Bytes int64
}

func (s Sum) String() string {
	return fmt.Sprintf("%d files, %d bytes, %s", s.Files, s.Bytes, s.Hash[:16])
}

// Of fingerprints dir.
func Of(dir string) (Sum, error) {
	h := sha256.New()
	var s Sum
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintf(h, "%s\x00%v\x00%d\x00%d\x00", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
			s.Files++
			s.Bytes += fi.Size()
		}
		return nil
	})
	s.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return s, err
}
