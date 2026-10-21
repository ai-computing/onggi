// Package digest names content by its SHA-256.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Digest is a SHA-256 of some bytes.
type Digest [32]byte

// Of returns the digest of b.
func Of(b []byte) Digest { return sha256.Sum256(b) }

// Hex is the 64-character lowercase hex form.
func (d Digest) Hex() string { return hex.EncodeToString(d[:]) }

// String is the "sha256:<hex>" form used in manifests and on the command line.
func (d Digest) String() string { return "sha256:" + d.Hex() }

// IsZero reports the zero digest.
func (d Digest) IsZero() bool { return d == Digest{} }

// Parse accepts "sha256:<hex>" or bare hex.
func Parse(s string) (Digest, error) {
	var d Digest
	h := strings.TrimPrefix(s, "sha256:")
	if len(h) != 64 {
		return d, fmt.Errorf("digest %q: want 64 hex characters", s)
	}
	if _, err := hex.Decode(d[:], []byte(h)); err != nil {
		return d, fmt.Errorf("digest %q: %v", s, err)
	}
	return d, nil
}

// MarshalText makes digests JSON strings.
func (d Digest) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses the JSON string form.
func (d *Digest) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	*d = v
	return err
}
