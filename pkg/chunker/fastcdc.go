// Package chunker splits a byte stream at content-defined boundaries (FastCDC, Xia et al. 2016).
//
// Boundaries depend only on the bytes around them, so the same content produces the same chunks
// wherever it sits in a file. That is what finds the weights an ATOM build repeats at shifted
// offsets (docs/artifact-formats.md); fixed-size blocks do not.
package chunker

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
)

// Params bounds chunk sizes. Avg must be a power of two.
type Params struct {
	Min, Avg, Max int
}

// Default is the setting the design measurements used: 16 KiB / 64 KiB / 256 KiB (ADR-0001).
var Default = Params{Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10}

// Validate reports a setting the chunker cannot use.
func (p Params) Validate() error {
	switch {
	case p.Min <= 0 || p.Avg <= 0 || p.Max <= 0:
		return errors.New("chunk sizes must be positive")
	case !(p.Min <= p.Avg && p.Avg <= p.Max):
		return fmt.Errorf("need min <= avg <= max, got %d/%d/%d", p.Min, p.Avg, p.Max)
	case p.Avg&(p.Avg-1) != 0:
		return fmt.Errorf("avg %d is not a power of two", p.Avg)
	case p.Avg < 64:
		return fmt.Errorf("avg %d is too small", p.Avg)
	}
	return nil
}

// masks returns the strict mask used before the average size and the loose one after it
// ("normalized chunking", level 2): chunk sizes cluster around Avg.
func (p Params) masks() (small, large uint64) {
	b := bits.TrailingZeros(uint(p.Avg))
	top := func(n int) uint64 { return ^uint64(0) << (64 - n) } // the gear hash mixes into its high bits
	return top(b + 2), top(b - 2)
}

// cut returns the length of the first chunk of data. len(data) is at most Max, or data is the
// rest of the stream.
func (p Params) cut(data []byte, small, large uint64) int {
	n := len(data)
	if n <= p.Min {
		return n
	}
	if n > p.Max {
		n = p.Max
	}
	normal := p.Avg
	if n < normal {
		normal = n
	}
	var fp uint64
	i := p.Min
	for ; i < normal; i++ {
		fp = fp<<1 + gear[data[i]]
		if fp&small == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = fp<<1 + gear[data[i]]
		if fp&large == 0 {
			return i + 1
		}
	}
	return n
}

// Chunker reads chunks from a stream. The slice returned by Next is valid until the next call.
type Chunker struct {
	r            io.Reader
	p            Params
	small, large uint64
	buf          []byte
	start, end   int
	eof          bool
}

// New returns a chunker over r. It panics on invalid params; check them with Validate first.
func New(r io.Reader, p Params) *Chunker {
	if err := p.Validate(); err != nil {
		panic(err)
	}
	s, l := p.masks()
	size := 4 * p.Max
	if size < 8<<20 {
		size = 8 << 20
	}
	return &Chunker{r: r, p: p, small: s, large: l, buf: make([]byte, size)}
}

// Next returns the next chunk, or io.EOF after the last one.
func (c *Chunker) Next() ([]byte, error) {
	if c.end-c.start < c.p.Max && !c.eof {
		if err := c.fill(); err != nil {
			return nil, err
		}
	}
	if c.start == c.end {
		return nil, io.EOF
	}
	n := c.p.cut(c.buf[c.start:c.end], c.small, c.large)
	chunk := c.buf[c.start : c.start+n]
	c.start += n
	return chunk, nil
}

func (c *Chunker) fill() error {
	copy(c.buf, c.buf[c.start:c.end])
	c.end -= c.start
	c.start = 0
	for c.end < len(c.buf) {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if err == io.EOF {
			c.eof = true
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}
