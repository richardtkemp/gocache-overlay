// Package overlay implements the GOCACHEPROG protocol (cmd/go/internal/cacheprog)
// as a read-through overlay: gets are served from a scratch store first and
// then from a shared native GOCACHE directory; puts go only to the scratch
// store. The shared directory is held as a native.Cache, which has no write
// methods.
package overlay

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/richardtkemp/gocache-overlay/internal/native"
)

// Wire types, mirroring cmd/go/internal/cacheprog.
type request struct {
	ID       int64
	Command  string
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

type response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

// Stats counts what one helper process did.
type Stats struct {
	Gets        int64 `json:"gets"`
	ScratchHits int64 `json:"scratch_hits"`
	SharedHits  int64 `json:"shared_hits"`
	Misses      int64 `json:"misses"`
	Puts        int64 `json:"puts"`
	PutBytes    int64 `json:"put_bytes"`
}

// Server answers one go command's requests.
type Server struct {
	Shared  native.Cache // read only
	Scratch native.Store // all writes land here
	// Touch, if set, is called (at most once a minute) after a put, so a
	// long-lived run dir is not taken for abandoned by the age sweep.
	Touch func()
	// Debug, if set, receives one line per get and put.
	Debug io.Writer

	mu        sync.Mutex
	stats     Stats
	lastTouch time.Time
}

// ErrFormat is returned by Serve when the shared cache is not in a format
// this helper understands. Serve stops at once: a silent miss on every entry
// would look like a working (just slow) cache.
var ErrFormat = native.ErrFormat

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Serve runs the protocol until a "close" request or EOF on in.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	bw := bufio.NewWriter(out)
	enc := json.NewEncoder(bw)
	send := func(r *response) error {
		if err := enc.Encode(r); err != nil {
			return err
		}
		return bw.Flush()
	}
	if err := send(&response{KnownCommands: []string{"get", "put", "close"}}); err != nil {
		return err
	}
	dec := json.NewDecoder(bufio.NewReaderSize(in, 1<<16))
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading request: %w", err)
		}
		var body []byte
		if req.Command == "put" && req.BodySize > 0 {
			if err := dec.Decode(&body); err != nil {
				return fmt.Errorf("reading put body for request %d: %w", req.ID, err)
			}
			if int64(len(body)) != req.BodySize {
				return fmt.Errorf("put %d: body is %d bytes, BodySize %d", req.ID, len(body), req.BodySize)
			}
		}
		var res *response
		var fatal error
		switch req.Command {
		case "get":
			res, fatal = s.get(&req)
		case "put":
			res = s.put(&req, body)
		case "close":
			return send(&response{ID: req.ID})
		default:
			res = &response{ID: req.ID, Err: fmt.Sprintf("unknown command %q", req.Command)}
		}
		if fatal != nil {
			return fatal
		}
		if err := send(res); err != nil {
			return err
		}
	}
}

func (s *Server) count(f func(*Stats)) {
	s.mu.Lock()
	f(&s.stats)
	s.mu.Unlock()
}

func (s *Server) get(req *request) (*response, error) {
	s.count(func(st *Stats) { st.Gets++ })
	// Scratch first: it holds this run's own outputs.
	for _, src := range []struct {
		c   native.Cache
		hit func(*Stats)
	}{
		{s.Scratch.Cache, func(st *Stats) { st.ScratchHits++ }},
		{s.Shared, func(st *Stats) { st.SharedHits++ }},
	} {
		e, path, err := src.c.Get(req.ActionID)
		if err == nil {
			s.count(src.hit)
			s.debugf("get %x hit %s", req.ActionID, path)
			t := e.Time
			return &response{ID: req.ID, OutputID: e.OutputID[:], Size: e.Size, Time: &t, DiskPath: path}, nil
		}
		if errors.Is(err, native.ErrFormat) {
			return nil, fmt.Errorf("%s: action %x: %w", src.c.Dir, req.ActionID, err)
		}
	}
	s.count(func(st *Stats) { st.Misses++ })
	s.debugf("get %x miss", req.ActionID)
	return &response{ID: req.ID, Miss: true}, nil
}

func (s *Server) put(req *request, body []byte) *response {
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], req.OutputID) {
		return &response{ID: req.ID, Err: fmt.Sprintf("put: OutputID %x is not the SHA-256 of the body", req.OutputID)}
	}
	path, err := s.Scratch.Put(req.ActionID, req.OutputID, body, time.Now())
	s.debugf("put %x %d bytes err=%v", req.ActionID, len(body), err)
	if err != nil {
		return &response{ID: req.ID, Err: err.Error()}
	}
	s.mu.Lock()
	s.stats.Puts++
	s.stats.PutBytes += int64(len(body))
	touch := s.Touch != nil && time.Since(s.lastTouch) > time.Minute
	if touch {
		s.lastTouch = time.Now()
	}
	s.mu.Unlock()
	if touch {
		s.Touch()
	}
	return &response{ID: req.ID, DiskPath: path}
}

func (s *Server) debugf(format string, a ...any) {
	if s.Debug != nil {
		fmt.Fprintf(s.Debug, "gocache-overlay: "+format+"\n", a...)
	}
}
