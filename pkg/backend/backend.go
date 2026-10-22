// Package backend is the one interface between onggi and durable storage (ADR-0008).
//
// Objects are immutable and named by what they are: a key is a kind plus a name, and for every
// content kind the name is the hex digest of the bytes. A backend therefore needs only atomic
// creation, read-after-write for one object, and a listing. Durability is the backend's; it only
// declares what it offers.
package backend

import (
	"context"
	"errors"
	"io"
)

// Kind is a key namespace.
type Kind string

const (
	Pack     Kind = "packs"     // concatenated chunks
	Index    Kind = "index"     // where each chunk of one pack lies
	List     Kind = "lists"     // a file's ordered chunk list
	Manifest Kind = "manifests" // one artifact: files and metadata
	Config   Kind = "config"    // store settings, written once
)

// Key names one object.
type Key struct {
	Kind Kind
	Name string
}

func (k Key) String() string { return string(k.Kind) + "/" + k.Name }

// ErrNotFound is returned by Get for a missing key.
var ErrNotFound = errors.New("not found")

// Durability is what a backend declares about keeping data.
type Durability string

const (
	DurabilityNone       Durability = "none"
	DurabilityReplicated Durability = "replicated"
	DurabilityErasure    Durability = "erasure-coded"
	DurabilityUnknown    Durability = "unknown"
)

// Capabilities describes a backend; the core works around what is missing.
type Capabilities struct {
	RangeReads   bool
	AtomicCreate bool // a second Put of the same key can never expose partial or mixed bytes
	Durability   Durability
}

// Backend stores immutable objects.
type Backend interface {
	// Put creates key with size bytes from r. It is atomic, and a no-op if the key exists.
	Put(ctx context.Context, key Key, r io.Reader, size int64) error
	// Get reads n bytes from off; n < 0 reads to the end. A missing key gives ErrNotFound.
	Get(ctx context.Context, key Key, off, n int64) (io.ReadCloser, error)
	// Has reports which keys exist.
	Has(ctx context.Context, keys []Key) ([]bool, error)
	// List calls fn for every key of a kind, with its size.
	List(ctx context.Context, kind Kind, fn func(name string, size int64) error) error
	// Delete removes key; deleting a missing key is not an error. Only GC deletes.
	Delete(ctx context.Context, key Key) error
	Caps() Capabilities
}

// Refs names artifacts. P1 keeps refs next to the data for the CLI; P3 moves them into the
// Kubernetes API (ADR-0007), where the same compare-and-swap is a resourceVersion check.
type Refs interface {
	// GetRef returns the value of name, or ErrNotFound.
	GetRef(ctx context.Context, name string) (string, error)
	// SetRef sets name to value if its current value is old ("" = must not exist). An empty
	// value deletes the ref, under the same compare-and-swap.
	SetRef(ctx context.Context, name, old, value string) error
	// ListRefs lists all refs.
	ListRefs(ctx context.Context, fn func(name, value string) error) error
}

// ErrConflict is returned by SetRef when the current value is not old.
var ErrConflict = errors.New("ref changed concurrently")
