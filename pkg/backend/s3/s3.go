// Package s3 is the S3-compatible backend: Ceph RGW, SeaweedFS, Garage, RustFS, VersityGW, cloud
// object storage.
//
// Objects live under <prefix>/<kind>/<first two hex>/<name>; refs under <prefix>/refs/<name>.
// A PUT becomes visible whole or not at all, so Put is atomic. Create-only and compare-and-swap
// use conditional writes (If-None-Match: *, If-Match: <etag>) when the server supports them; a
// server that ignores them still stores correct content objects (same key, same bytes), but its
// refs are last-writer-wins, and Caps reports that.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/ai-computing/onggi/pkg/backend"
)

// Store is an S3 backend.
type Store struct {
	c          *minio.Client
	bucket     string
	prefix     string
	durability backend.Durability
}

// Open parses s3://bucket/prefix?endpoint=host:port&insecure=1&region=r. Credentials come from
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or the usual AWS environment and files).
func Open(raw string, durability backend.Durability) (*Store, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("store %q: want s3://bucket[/prefix]?endpoint=host:port", raw)
	}
	q := u.Query()
	endpoint := q.Get("endpoint")
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	creds := credentials.NewChainCredentials([]credentials.Provider{&credentials.EnvAWS{}, &credentials.FileAWSCredentials{}, &credentials.IAM{}})
	c, err := minio.New(endpoint, &minio.Options{Creds: creds, Secure: q.Get("insecure") == "", Region: q.Get("region")})
	if err != nil {
		return nil, err
	}
	if durability == "" {
		durability = backend.DurabilityUnknown
	}
	return &Store{c: c, bucket: u.Host, prefix: strings.Trim(u.Path, "/"), durability: durability}, nil
}

func (s *Store) key(k backend.Key) (string, error) {
	if k.Name == "" || strings.ContainsAny(k.Name, `/\`) || strings.ContainsAny(string(k.Kind), `/\.`) {
		return "", fmt.Errorf("bad key %q", k)
	}
	shard := k.Name
	if len(shard) > 2 {
		shard = shard[:2]
	}
	return path.Join(s.prefix, string(k.Kind), shard, k.Name), nil
}

func notFound(err error) bool {
	var r minio.ErrorResponse
	if errors.As(err, &r) {
		return r.Code == "NoSuchKey" || r.StatusCode == 404
	}
	return false
}

func preconditionFailed(err error) bool {
	var r minio.ErrorResponse
	if errors.As(err, &r) {
		return r.Code == "PreconditionFailed" || r.StatusCode == 412
	}
	return false
}

// Put uploads with If-None-Match: *; an existing key is success (content keys hold the same bytes).
func (s *Store) Put(ctx context.Context, k backend.Key, r io.Reader, size int64) error {
	key, err := s.key(k)
	if err != nil {
		return err
	}
	if _, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: size <= 512<<20, PartSize: 64 << 20}
	opts.SetMatchETagExcept("*")
	// r is passed as is: minio-go rewinds a seekable reader to retry, and a body shorter than size
	// fails the request (Content-Length), so no partial object can appear.
	_, err = s.c.PutObject(ctx, s.bucket, key, r, size, opts)
	if err == nil || preconditionFailed(err) {
		return nil
	}
	// A server may answer a losing conditional PUT by closing the connection mid-body. If the key
	// exists now, another writer published the same bytes.
	if _, serr := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); serr == nil {
		return nil
	}
	return err
}

// Get reads a range; n < 0 reads to the end.
func (s *Store) Get(ctx context.Context, k backend.Key, off, n int64) (io.ReadCloser, error) {
	key, err := s.key(k)
	if err != nil {
		return nil, err
	}
	var opts minio.GetObjectOptions
	switch {
	case n > 0:
		err = opts.SetRange(off, off+n-1)
	case n == 0:
		return io.NopCloser(strings.NewReader("")), nil
	case off > 0:
		err = opts.SetRange(off, 0)
	}
	if err != nil {
		return nil, err
	}
	// minio.Core sends the request now, with the range: a missing key fails here. (The high-level
	// Object would drop the Range header if Stat were called before the first Read.)
	body, _, _, err := minio.Core{Client: s.c}.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		if notFound(err) {
			return nil, fmt.Errorf("%s: %w", k, backend.ErrNotFound)
		}
		return nil, err
	}
	return body, nil
}

// Has stats keys, a few at a time.
func (s *Store) Has(ctx context.Context, keys []backend.Key) ([]bool, error) {
	out := make([]bool, len(keys))
	errs := make([]error, len(keys))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, k := range keys {
		key, err := s.key(k)
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
			switch {
			case err == nil:
				out[i] = true
			case !notFound(err):
				errs[i] = err
			}
		}(i, key)
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

// List lists one kind.
func (s *Store) List(ctx context.Context, kind backend.Kind, fn func(string, int64) error) error {
	pfx := path.Join(s.prefix, string(kind)) + "/"
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: pfx, Recursive: true}) {
		if o.Err != nil {
			return o.Err
		}
		if err := fn(path.Base(o.Key), o.Size); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes an object; a missing one is not an error.
func (s *Store) Delete(ctx context.Context, k backend.Key) error {
	key, err := s.key(k)
	if err != nil {
		return err
	}
	return s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// Caps: S3 PUTs are atomic and ranged GETs exist.
func (s *Store) Caps() backend.Capabilities {
	return backend.Capabilities{RangeReads: true, AtomicCreate: true, Durability: s.durability}
}

// ---- refs: compare-and-swap with conditional writes.

func (s *Store) refKey(name string) (string, error) {
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `\`) {
		return "", fmt.Errorf("bad ref name %q", name)
	}
	return path.Join(s.prefix, "refs", url.PathEscape(name)), nil
}

func (s *Store) readRef(ctx context.Context, key string) (value, etag string, err error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return "", "", err
	}
	defer obj.Close()
	info, err := obj.Stat()
	if err != nil {
		return "", "", err
	}
	b, err := io.ReadAll(obj)
	return strings.TrimSpace(string(b)), info.ETag, err
}

func (s *Store) GetRef(ctx context.Context, name string) (string, error) {
	key, err := s.refKey(name)
	if err != nil {
		return "", err
	}
	v, _, err := s.readRef(ctx, key)
	if notFound(err) {
		return "", fmt.Errorf("ref %s: %w", name, backend.ErrNotFound)
	}
	return v, err
}

func (s *Store) SetRef(ctx context.Context, name, old, value string) error {
	key, err := s.refKey(name)
	if err != nil {
		return err
	}
	cur, etag, err := s.readRef(ctx, key)
	if notFound(err) {
		cur, etag, err = "", "", nil
	}
	if err != nil {
		return err
	}
	if cur != old {
		return fmt.Errorf("ref %s is %q, expected %q: %w", name, cur, old, backend.ErrConflict)
	}
	opts := minio.PutObjectOptions{ContentType: "text/plain"}
	if etag == "" {
		opts.SetMatchETagExcept("*")
	} else {
		opts.SetMatchETag(etag)
	}
	body := value + "\n"
	_, err = s.c.PutObject(ctx, s.bucket, key, strings.NewReader(body), int64(len(body)), opts)
	if preconditionFailed(err) {
		return fmt.Errorf("ref %s changed during the update: %w", name, backend.ErrConflict)
	}
	return err
}

func (s *Store) ListRefs(ctx context.Context, fn func(name, value string) error) error {
	pfx := path.Join(s.prefix, "refs") + "/"
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: pfx, Recursive: true}) {
		if o.Err != nil {
			return o.Err
		}
		name, err := url.PathUnescape(path.Base(o.Key))
		if err != nil {
			return err
		}
		v, _, err := s.readRef(ctx, o.Key)
		if err != nil {
			return err
		}
		if err := fn(name, v); err != nil {
			return err
		}
	}
	return nil
}

// EnsureBucket creates the bucket if it does not exist (tests, first use).
func (s *Store) EnsureBucket(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil || ok {
		return err
	}
	return s.c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}

// FromEnv opens ONGGI_TEST_S3 for tests (an s3:// URL), or returns "" when unset.
func FromEnv() string { return os.Getenv("ONGGI_TEST_S3") }
