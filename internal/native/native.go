// Package native reads the go command's on-disk build cache (the directory
// named by GOCACHE) and writes the same format into scratch directories.
//
// Cache is read-only: it has no method that writes. Writing needs a Store,
// which the overlay only ever builds over a scratch directory.
//
// The format is not documented as stable. It is mirrored here from
// cmd/go/internal/cache (stable through at least Go 1.26):
//
//	<dir>/<hex(id)[:2]>/<hex(id)>-a   action entry, one line:
//	    "v1 <hex actionID> <hex outputID> <size, %20d> <unixnano, %20d>\n"
//	<dir>/<hex(out)[:2]>/<hex(out)>-d output body (a regular file), or a
//	    directory holding exactly one cached executable.
//
// Anything that looks like a different format is reported as ErrFormat so
// callers can fail loudly instead of silently missing every lookup.
package native

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// HashSize is the size of action and output IDs (SHA-256).
const HashSize = 32

const (
	hexSize = HashSize * 2
	// EntrySize is the exact length of an action entry file.
	EntrySize = 2 + 1 + hexSize + 1 + hexSize + 1 + 20 + 1 + 20 + 1
)

// ErrFormat means an entry exists but is not in the format this package
// understands: most likely the go command changed its cache layout.
var ErrFormat = errors.New("native Go build cache format not recognised (Go upgrade? see README)")

// ErrMiss means there is no usable entry (absent, partially written, or its
// body is missing or the wrong size). It is not a format problem.
var ErrMiss = errors.New("cache miss")

// Entry is a parsed action entry.
type Entry struct {
	OutputID [HashSize]byte
	Size     int64
	Time     time.Time
}

// Cache is a read-only view of a native cache directory.
type Cache struct {
	Dir string
}

func (c Cache) fileName(id []byte, suffix string) string {
	return filepath.Join(c.Dir, fmt.Sprintf("%02x", id[0]), fmt.Sprintf("%x", id)+"-"+suffix)
}

// ParseEntry parses the contents of an "-a" file. want, if non-nil, must
// match the action ID recorded in the entry.
//
// A short file that is still a prefix of a valid entry is ErrMiss: the
// native writer creates the file before writing it, so a concurrent reader
// can see it empty or partial. Everything else that fails to parse is
// ErrFormat.
func ParseEntry(data []byte, want []byte) (Entry, error) {
	if len(data) < EntrySize {
		if bytes.HasPrefix([]byte("v1 "), data) || bytes.HasPrefix(data, []byte("v1 ")) {
			return Entry{}, fmt.Errorf("%w: entry file incomplete (%d bytes)", ErrMiss, len(data))
		}
		return Entry{}, fmt.Errorf("%w: entry is %d bytes and does not start with \"v1 \"", ErrFormat, len(data))
	}
	if len(data) > EntrySize {
		return Entry{}, fmt.Errorf("%w: entry is %d bytes, want %d", ErrFormat, len(data), EntrySize)
	}
	if data[0] != 'v' || data[1] != '1' || data[2] != ' ' ||
		data[3+hexSize] != ' ' ||
		data[3+hexSize+1+hexSize] != ' ' ||
		data[3+hexSize+1+hexSize+1+20] != ' ' ||
		data[EntrySize-1] != '\n' {
		return Entry{}, fmt.Errorf("%w: invalid header %q", ErrFormat, data[:min(len(data), 16)])
	}
	eid := data[3 : 3+hexSize]
	rest := data[3+hexSize:]
	eout, rest := rest[1:1+hexSize], rest[1+hexSize:]
	esize, rest := rest[1:1+20], rest[1+20:]
	etime := rest[1 : 1+20]

	var id, out [HashSize]byte
	if _, err := hex.Decode(id[:], eid); err != nil {
		return Entry{}, fmt.Errorf("%w: decoding action ID: %v", ErrFormat, err)
	}
	if want != nil && !bytes.Equal(id[:], want) {
		// The native reader treats this as a miss too; it is a corrupt or
		// misplaced file, not a layout change.
		return Entry{}, fmt.Errorf("%w: mismatched action ID", ErrMiss)
	}
	if _, err := hex.Decode(out[:], eout); err != nil {
		return Entry{}, fmt.Errorf("%w: decoding output ID: %v", ErrFormat, err)
	}
	size, err := strconv.ParseInt(string(bytes.TrimLeft(esize, " ")), 10, 64)
	if err != nil || size < 0 {
		return Entry{}, fmt.Errorf("%w: bad size field %q", ErrFormat, esize)
	}
	tm, err := strconv.ParseInt(string(bytes.TrimLeft(etime, " ")), 10, 64)
	if err != nil || tm < 0 {
		return Entry{}, fmt.Errorf("%w: bad time field %q", ErrFormat, etime)
	}
	return Entry{OutputID: out, Size: size, Time: time.Unix(0, tm)}, nil
}

// readEntryFile reads at most EntrySize+1 bytes, enough to detect a long file.
func readEntryFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, EntrySize+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// Get looks up actionID. On a hit it returns the entry and the path of the
// body file. It never modifies the directory (unlike the go command, it does
// not refresh mtimes).
func (c Cache) Get(actionID []byte) (Entry, string, error) {
	if len(actionID) != HashSize {
		return Entry{}, "", fmt.Errorf("%w: action ID is %d bytes", ErrMiss, len(actionID))
	}
	data, err := readEntryFile(c.fileName(actionID, "a"))
	if err != nil {
		return Entry{}, "", fmt.Errorf("%w: %w", ErrMiss, err)
	}
	e, err := ParseEntry(data, actionID)
	if err != nil {
		return Entry{}, "", err
	}
	path, err := c.bodyPath(e)
	if err != nil {
		return Entry{}, "", err
	}
	return e, path, nil
}

// bodyPath resolves the "-d" file for e, following the cached-executable
// layout (a directory with exactly one file), and checks its size.
func (c Cache) bodyPath(e Entry) (string, error) {
	path := c.fileName(e.OutputID[:], "d")
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrMiss, err)
	}
	if fi.IsDir() {
		ents, err := os.ReadDir(path)
		if err != nil || len(ents) != 1 {
			return "", fmt.Errorf("%w: invalid cached-executable dir %s", ErrMiss, path)
		}
		path = filepath.Join(path, ents[0].Name())
		if fi, err = os.Stat(path); err != nil {
			return "", fmt.Errorf("%w: %w", ErrMiss, err)
		}
	}
	if !fi.Mode().IsRegular() || fi.Size() != e.Size {
		return "", fmt.Errorf("%w: body %s is %d bytes, entry says %d", ErrMiss, path, fi.Size(), e.Size)
	}
	return path, nil
}

// Probe samples up to max action entries and returns ErrFormat if any of
// them is unparseable. It returns the number of entries that parsed. An
// empty cache probes as (0, nil).
func (c Cache) Probe(max int) (int, error) {
	ok := 0
	seen := 0
	for i := 0; i < 256 && seen < max; i++ {
		sub := filepath.Join(c.Dir, fmt.Sprintf("%02x", i))
		f, err := os.Open(sub)
		if err != nil {
			continue
		}
		ents, _ := f.ReadDir(256)
		f.Close()
		for _, de := range ents {
			name := de.Name()
			if len(name) < 2 || name[len(name)-2:] != "-a" {
				continue
			}
			data, err := readEntryFile(filepath.Join(sub, name))
			if err != nil {
				continue
			}
			seen++
			if _, err := ParseEntry(data, nil); err != nil {
				if errors.Is(err, ErrFormat) {
					return ok, fmt.Errorf("%s: %w", filepath.Join(sub, name), err)
				}
			} else {
				ok++
			}
			if seen >= max {
				break
			}
		}
	}
	return ok, nil
}
