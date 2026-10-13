package native

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func id(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func entryLine(a, o []byte, size int64) []byte {
	return []byte(fmt.Sprintf("v1 %x %x %20d %20d\n", a, o, size, time.Now().UnixNano()))
}

func TestParseEntry(t *testing.T) {
	a, o := id("a"), id("o")
	good := entryLine(a, o, 42)
	if len(good) != EntrySize {
		t.Fatalf("test line is %d bytes, EntrySize %d", len(good), EntrySize)
	}
	e, err := ParseEntry(good, a)
	if err != nil || e.Size != 42 || !bytes.Equal(e.OutputID[:], o) {
		t.Fatalf("good entry: %+v, %v", e, err)
	}

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty (writer created, not yet written)", nil, ErrMiss},
		{"partial v1", good[:50], ErrMiss},
		{"other action id", entryLine(id("b"), o, 1), ErrMiss},
		{"v2 version tag", append([]byte("v2"), good[2:]...), ErrFormat},
		{"longer line", append(bytes.TrimSuffix(good, []byte("\n")), []byte(" extra\n")...), ErrFormat},
		{"shorter line, new layout", []byte("x1 deadbeef\n"), ErrFormat},
		{"bad separator", append(append([]byte{}, good[:3+64]...), append([]byte("_"), good[3+64+1:]...)...), ErrFormat},
		{"non-hex output id", bytes.Replace(good, []byte(fmt.Sprintf("%x", o)), bytes.Repeat([]byte("z"), 64), 1), ErrFormat},
	}
	for _, c := range cases {
		if _, err := ParseEntry(c.data, a); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

func TestStorePutGetAndExecutableDir(t *testing.T) {
	dir := t.TempDir()
	s := Store{Cache: Cache{Dir: dir}}
	body := []byte("hello")
	out := sha256.Sum256(body)
	path, err := s.Put(id("x"), out[:], body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e, got, err := s.Get(id("x"))
	if err != nil || got != path || e.Size != 5 {
		t.Fatalf("get: %+v %q %v (put path %q)", e, got, err, path)
	}
	if _, _, err := s.Get(id("absent")); !errors.Is(err, ErrMiss) {
		t.Fatalf("absent: %v", err)
	}

	// Native cached executables: "-d" is a directory holding one file.
	exe := []byte("ELF...")
	eout := sha256.Sum256(exe)
	d := s.fileName(eout[:], "d")
	if err := os.MkdirAll(d, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "prog"), exe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(s.fileName(id("link"), "a"), entryLine(id("link"), eout[:], int64(len(exe))), 0o666); err != nil {
		t.Fatal(err)
	}
	_, got, err = s.Get(id("link"))
	if err != nil || got != filepath.Join(d, "prog") {
		t.Fatalf("exe dir: %q %v", got, err)
	}

	// A body of the wrong size (e.g. still being written natively) is a miss.
	if err := writeFile(s.fileName(id("short"), "a"), entryLine(id("short"), out[:], 99), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(id("short")); !errors.Is(err, ErrMiss) {
		t.Fatalf("size mismatch: %v", err)
	}
}

func TestProbe(t *testing.T) {
	dir := t.TempDir()
	s := Store{Cache: Cache{Dir: dir}}
	if n, err := s.Probe(16); n != 0 || err != nil {
		t.Fatalf("empty: %d %v", n, err)
	}
	body := []byte("b")
	out := sha256.Sum256(body)
	for i := range 3 {
		if _, err := s.Put(id(fmt.Sprint(i)), out[:], body, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Probe(16); n != 3 || err != nil {
		t.Fatalf("good: %d %v", n, err)
	}
	bad := id("future")
	if err := writeFile(s.fileName(bad, "a"), []byte("v2 something else entirely\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Probe(16); !errors.Is(err, ErrFormat) {
		t.Fatalf("planted v2 entry: want ErrFormat, got %v", err)
	}
}

// TestConcurrentWriters: several writers (as several go processes sharing a
// run id would) put overlapping keys while readers read them. A reader may
// miss, but must never see a partial or malformed entry or body.
func TestConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	const writers, keys, rounds = 8, 32, 20
	bodies := make([][]byte, keys)
	for k := range bodies {
		bodies[k] = bytes.Repeat([]byte{byte(k)}, 64<<10+k) // 64 KiB+: not one write() on every fs
	}
	stop := make(chan struct{})
	var readErr error
	var rmu sync.Mutex
	var rwg sync.WaitGroup
	for range 4 {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			c := Cache{Dir: dir}
			for {
				select {
				case <-stop:
					return
				default:
				}
				for k := range keys {
					e, path, err := c.Get(id(fmt.Sprint(k)))
					if err != nil {
						if errors.Is(err, fs.ErrNotExist) {
							continue // not written yet: the only acceptable miss
						}
						rmu.Lock()
						readErr = fmt.Errorf("key %d: %v", k, err)
						rmu.Unlock()
						return
					}
					data, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(data, bodies[k]) || e.Size != int64(len(bodies[k])) {
						rmu.Lock()
						readErr = fmt.Errorf("key %d: torn body (%d bytes, %v)", k, len(data), err)
						rmu.Unlock()
						return
					}
				}
			}
		}()
	}
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := Store{Cache: Cache{Dir: dir}}
			for r := range rounds {
				for k := range keys {
					k := (k + w + r) % keys
					out := sha256.Sum256(bodies[k])
					if _, err := s.Put(id(fmt.Sprint(k)), out[:], bodies[k], time.Now()); err != nil {
						t.Errorf("writer %d: %v", w, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	rwg.Wait()
	if readErr != nil {
		t.Fatal(readErr)
	}
	// No temp files left behind, every key complete.
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*", ".tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
	for k := range keys {
		if _, _, err := (Cache{Dir: dir}).Get(id(fmt.Sprint(k))); err != nil {
			t.Fatalf("key %d after writers: %v", k, err)
		}
	}
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}
