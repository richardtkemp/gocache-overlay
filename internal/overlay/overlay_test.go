package overlay

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richardtkemp/gocache-overlay/internal/native"
	"github.com/richardtkemp/gocache-overlay/internal/treehash"
)

func hash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// client drives a Server over pipes the way the go command does.
type client struct {
	t   *testing.T
	in  *io.PipeWriter
	dec *json.Decoder
	id  int64
	err chan error
}

func newClient(t *testing.T, s *Server) *client {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, in: inW, dec: json.NewDecoder(bufio.NewReader(outR)), err: make(chan error, 1)}
	go func() { c.err <- s.Serve(inR, outW); outW.Close() }()
	var hello response
	if err := c.dec.Decode(&hello); err != nil || len(hello.KnownCommands) != 3 {
		t.Fatalf("hello: %+v %v", hello, err)
	}
	return c
}

func (c *client) send(req request, body []byte) response {
	c.t.Helper()
	c.id++
	req.ID = c.id
	b, _ := json.Marshal(req)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	if len(body) > 0 {
		b, _ = json.Marshal(body) // base64 JSON string, as cmd/go sends it
		if _, err := c.in.Write(append(b, '\n')); err != nil {
			c.t.Fatal(err)
		}
	}
	var res response
	if err := c.dec.Decode(&res); err != nil {
		c.t.Fatalf("response to %d: %v (serve: %v)", req.ID, err, <-c.err)
	}
	if res.ID != req.ID {
		c.t.Fatalf("response ID %d for request %d", res.ID, req.ID)
	}
	return res
}

func (c *client) put(action string, body []byte) response {
	out := sha256.Sum256(body)
	return c.send(request{Command: "put", ActionID: hash(action), OutputID: out[:], BodySize: int64(len(body))}, body)
}

func (c *client) get(action string) response {
	return c.send(request{Command: "get", ActionID: hash(action)}, nil)
}

func newServer(t *testing.T) (*Server, string, string) {
	shared, scratch := t.TempDir(), t.TempDir()
	return &Server{Shared: native.Cache{Dir: shared}, Scratch: native.Store{Cache: native.Cache{Dir: scratch}}}, shared, scratch
}

// Requirement 3: an entry put earlier in one invocation can be got later.
func TestPutThenGetSameInvocation(t *testing.T) {
	s, _, scratch := newServer(t)
	c := newClient(t, s)
	if r := c.get("a"); !r.Miss {
		t.Fatalf("before put: %+v", r)
	}
	body := []byte("compiled output")
	pr := c.put("a", body)
	if pr.Err != "" || pr.DiskPath == "" {
		t.Fatalf("put: %+v", pr)
	}
	gr := c.get("a")
	if gr.Miss || gr.Size != int64(len(body)) {
		t.Fatalf("get after put: %+v", gr)
	}
	if data, _ := os.ReadFile(gr.DiskPath); string(data) != string(body) {
		t.Fatalf("DiskPath %s holds %q", gr.DiskPath, data)
	}
	if rel, _ := filepath.Rel(scratch, gr.DiskPath); rel == "" || rel[0] == '.' {
		t.Fatalf("DiskPath %s not under scratch %s", gr.DiskPath, scratch)
	}
	// Zero-length bodies are sent with no body line.
	if r := c.put("empty", nil); r.Err != "" {
		t.Fatalf("empty put: %+v", r)
	}
	if r := c.get("empty"); r.Miss || r.Size != 0 {
		t.Fatalf("empty get: %+v", r)
	}
	if r := c.send(request{Command: "close"}, nil); r.Err != "" {
		t.Fatal(r.Err)
	}
	if err := <-c.err; err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Puts != 2 || st.ScratchHits != 2 || st.Misses != 1 || st.SharedHits != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestSharedHitAndPutsStayOutOfShared(t *testing.T) {
	s, shared, _ := newServer(t)
	// Populate the shared dir natively (Store is used only to lay out files).
	body := []byte("std package")
	out := sha256.Sum256(body)
	if _, err := (native.Store{Cache: native.Cache{Dir: shared}}).Put(hash("std"), out[:], body, time.Now()); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, shared)
	c := newClient(t, s)
	if r := c.get("std"); r.Miss || filepath.Dir(filepath.Dir(r.DiskPath)) != shared {
		t.Fatalf("shared hit: %+v", r)
	}
	c.put("new", []byte("mutant output"))
	c.put("std", []byte("rebuilt differently")) // shadowing a shared key still goes to scratch
	if r := c.get("std"); r.Miss || filepath.Dir(filepath.Dir(r.DiskPath)) == shared {
		t.Fatalf("scratch should shadow shared: %+v", r)
	}
	c.in.Close()
	if err := <-c.err; err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, shared); after != before {
		t.Fatalf("shared dir changed:\nbefore %s\nafter  %s", before, after)
	}
}

func TestFormatMismatchIsFatal(t *testing.T) {
	s, shared, _ := newServer(t)
	a := hash("x")
	p := filepath.Join(shared, fmt.Sprintf("%02x", a[0]), fmt.Sprintf("%x-a", a))
	os.MkdirAll(filepath.Dir(p), 0o777)
	os.WriteFile(p, []byte("v2 a whole new layout\n"), 0o666)
	c := newClient(t, s)
	b, _ := json.Marshal(request{ID: 1, Command: "get", ActionID: a})
	c.in.Write(append(b, '\n'))
	err := <-c.err
	if err == nil || !errorsIs(err, native.ErrFormat) {
		t.Fatalf("want fatal ErrFormat, got %v", err)
	}
}

func TestBadOutputIDRejected(t *testing.T) {
	s, _, _ := newServer(t)
	c := newClient(t, s)
	r := c.send(request{Command: "put", ActionID: hash("a"), OutputID: hash("wrong"), BodySize: 3}, []byte("abc"))
	if r.Err == "" {
		t.Fatal("put with wrong OutputID accepted")
	}
}

func TestSweepAndRemoveRun(t *testing.T) {
	root := t.TempDir()
	old, _, err := OpenScratch(root, "old-run")
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, _ := OpenScratch(root, "fresh-run")
	inv, eph, _ := OpenScratch(root, "")
	if !eph {
		t.Fatal("id-less scratch must be ephemeral")
	}
	unrelated := filepath.Join(root, "not-ours")
	os.Mkdir(unrelated, 0o777)
	long := time.Now().Add(-48 * time.Hour)
	for _, d := range []string{old, inv, unrelated} {
		os.Chtimes(d, long, long)
	}
	removed, err := Sweep(root, 24*time.Hour, inv) // inv is "ours": kept despite age
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Fatalf("removed %v, want [%s]", removed, old)
	}
	for _, d := range []string{fresh, inv, unrelated} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s should survive: %v", d, err)
		}
	}
	if r, _ := Sweep(root, 0, ""); len(r) != 0 {
		t.Fatalf("max-age 0 must disable the sweep, removed %v", r)
	}

	os.WriteFile(filepath.Join(fresh, "f"), []byte("12345"), 0o644)
	n, err := RemoveRun(root, "fresh-run")
	if err != nil || n != 5 {
		t.Fatalf("RemoveRun: %d %v", n, err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("run dir still there: %v", err)
	}
	if _, err := RemoveRun(root, "fresh-run"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	for _, bad := range []string{"", "..", "../x", "a/b", ".hidden"} {
		if _, err := RemoveRun(root, bad); err == nil {
			t.Errorf("RemoveRun(%q) accepted", bad)
		}
	}
}

func snapshot(t *testing.T, dir string) string {
	t.Helper()
	s, err := treehash.Of(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s.String()
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }
