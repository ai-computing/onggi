// Package peer serves a node cache to other nodes and reads from theirs (#34, ADR-0003).
//
// Protocol, HTTP/1.1, all bodies binary:
//
//	POST /v1/has     body: n × digest [32]           response: n bytes, 1 = held as a stored chunk
//	POST /v1/chunks  body: n × digest [32]           response: one record per held chunk:
//	                 digest [32], codec uint8, raw length uint32, stored length uint32, stored bytes
//
// Integers are little-endian. The client checks nothing itself: the pull decodes every chunk and
// compares its hash, so a wrong or corrupt answer only costs a read from the store.
package peer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

const (
	maxHas    = 1 << 16
	maxChunks = 1 << 13
)

// Server serves a node cache. Open returns the cache store; the server reopens it every Refresh
// (default 30 s) so it sees packs that pulls added and stops naming packs that GC removed.
type Server struct {
	Open    func(context.Context) (*store.Store, error)
	Refresh time.Duration

	mu     sync.Mutex
	cur    *store.Store
	loaded time.Time
}

func (s *Server) store(ctx context.Context) (*store.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.Refresh
	if r <= 0 {
		r = 30 * time.Second
	}
	if s.cur != nil && time.Since(s.loaded) < r {
		return s.cur, nil
	}
	st, err := s.Open(ctx)
	if err == nil {
		err = st.LoadIndex(ctx)
	}
	if err != nil {
		return nil, err
	}
	s.cur, s.loaded = st, time.Now()
	return st, nil
}

func readDigests(r io.Reader, limit int) ([]digest.Digest, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)*32+1))
	if err != nil {
		return nil, err
	}
	if len(data)%32 != 0 || len(data) > limit*32 {
		return nil, fmt.Errorf("body must be at most %d digests of 32 bytes", limit)
	}
	out := make([]digest.Digest, len(data)/32)
	for i := range out {
		copy(out[i][:], data[i*32:])
	}
	return out, nil
}

// ServeHTTP implements the protocol above.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	limit := maxHas
	if r.URL.Path == "/v1/chunks" {
		limit = maxChunks
	} else if r.URL.Path != "/v1/has" {
		http.NotFound(w, r)
		return
	}
	ds, err := readDigests(r.Body, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, err := s.store(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.URL.Path == "/v1/has" {
		has, err := st.HasStored(r.Context(), ds)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]byte, len(has))
		for i, h := range has {
			if h {
				out[i] = 1
			}
		}
		w.Write(out)
		return
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	var hdr [41]byte
	st.ReadStored(r.Context(), ds, func(d digest.Digest, stored []byte, id codec.ID, rawLen uint32) error {
		copy(hdr[:32], d[:])
		hdr[32] = byte(id)
		binary.LittleEndian.PutUint32(hdr[33:], rawLen)
		binary.LittleEndian.PutUint32(hdr[37:], uint32(len(stored)))
		if _, err := bw.Write(hdr[:]); err != nil {
			return err
		}
		_, err := bw.Write(stored)
		return err
	})
	// a read error ends the stream early: the client sees fewer chunks and the store serves the rest
	bw.Flush()
}

// Client reads from one peer. It implements store.ChunkSource.
type Client struct {
	URL  string // e.g. http://10.0.0.2:7070 (no trailing slash)
	HTTP *http.Client
}

func (c *Client) Name() string { return c.URL }

func (c *Client) post(ctx context.Context, path string, ds []digest.Digest) (*http.Response, error) {
	body := make([]byte, 0, len(ds)*32)
	for _, d := range ds {
		body = append(body, d[:]...)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("peer %s%s: %s", c.URL, path, resp.Status)
	}
	return resp, nil
}

// Has asks which of ds the peer holds.
func (c *Client) Has(ctx context.Context, ds []digest.Digest) ([]bool, error) {
	out := make([]bool, 0, len(ds))
	for a := 0; a < len(ds); a += maxHas {
		b := min(a+maxHas, len(ds))
		resp, err := c.post(ctx, "/v1/has", ds[a:b])
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, int64(b-a)+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(data) != b-a {
			return nil, fmt.Errorf("peer %s: %d answers for %d digests", c.URL, len(data), b-a)
		}
		for _, x := range data {
			out = append(out, x == 1)
		}
	}
	return out, nil
}

// Fetch streams the chunks the peer holds among ds to fn.
func (c *Client) Fetch(ctx context.Context, ds []digest.Digest, fn func(digest.Digest, []byte, codec.ID, uint32) error) error {
	for a := 0; a < len(ds); a += maxChunks {
		b := min(a+maxChunks, len(ds))
		if err := c.fetch(ctx, ds[a:b], fn); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) fetch(ctx context.Context, ds []digest.Digest, fn func(digest.Digest, []byte, codec.ID, uint32) error) error {
	resp, err := c.post(ctx, "/v1/chunks", ds)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	br := bufio.NewReaderSize(resp.Body, 1<<20)
	var hdr [41]byte
	var buf []byte
	for n := 0; n <= len(ds); n++ {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var d digest.Digest
		copy(d[:], hdr[:32])
		size := binary.LittleEndian.Uint32(hdr[37:])
		if size > 1<<20 {
			return fmt.Errorf("peer %s: chunk of %d bytes", c.URL, size)
		}
		if cap(buf) < int(size) {
			buf = make([]byte, size)
		}
		b := buf[:size]
		if _, err := io.ReadFull(br, b); err != nil {
			return err
		}
		if err := fn(d, b, codec.ID(hdr[32]), binary.LittleEndian.Uint32(hdr[33:])); err != nil {
			return err
		}
	}
	return fmt.Errorf("peer %s: more chunks than asked for", c.URL)
}
