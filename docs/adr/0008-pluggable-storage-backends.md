# ADR-0008: Pluggable storage backends; durability delegated to the backend

Status: proposed (2026-10-21) · supersedes [ADR-0002](0002-durable-tier-is-an-existing-object-store.md)

## Context
ADR-0002 named one kind of durable tier (an erasure-coded object store). Sites already run different storage: the
lab keeps models on an SMB share, others have NFS, CephFS, JuiceFS, Lustre/GPFS, S3-compatible stores or container
registries. Onggi's data is immutable and named by content hash, and its only mutable state (refs) lives in the
Kubernetes API (ADR-0007). So the backend needs very little: atomic creation of an object, read-after-write for that
object, and a listing for GC. Every byte is hash-verified when it reaches a node, so a weak backend cannot hand an
engine wrong data.

## Decision
1. **One narrow interface.** The core talks to storage only through:

   ```go
   type Backend interface {
       Put(ctx context.Context, key Digest, r io.Reader, size int64) error // atomic; a no-op if the key exists
       Get(ctx context.Context, key Digest, off, n int64) (io.ReadCloser, error)
       Has(ctx context.Context, keys []Digest) ([]bool, error)             // batched existence check
       List(ctx context.Context, prefix string, fn func(Digest, int64) error) error
       Delete(ctx context.Context, key Digest) error                       // GC only
       Caps() Capabilities
   }
   ```

   `Capabilities` declares range reads, server-side copy, presigned URLs, atomic create (rename or conditional put),
   and a **durability class** (`none`, `replicated`, `erasure-coded`, `unknown`).
2. **Drivers.** `fs` (any POSIX mount: local disk, NFS, SMB, CephFS, JuiceFS, Lustre/GPFS), `s3` (MinIO, Ceph RGW,
   cloud), `oci` (registries, OCI artifact layout). More (native RADOS, …) only when a site needs them.
3. **Durability is the backend's job.** Onggi never replicates within a backend. It reports the declared durability
   class, and `StorePolicy` can require one (for example, refuse `none` for the durable tier).
4. **Selection per policy.** A `StorePolicy` names one or more backends per tier (hot artifacts on CephFS, superseded
   SDK builds on S3). Moving between backends is a copy of chunks by digest, then a policy change; refs do not move.
5. **Missing capabilities are worked around in the core, not in drivers:** no atomic rename → write to a temporary
   name, verify the hash, then create; no conditional put → `Has` first and treat a concurrent duplicate as success
   (same digest, same bytes).
6. **One conformance suite** runs against every driver: atomic create under concurrent writers of the same key,
   idempotent Put, partial-write cleanup after a crash, range reads, listing completeness, Delete of a missing key.
   A storage system is supported when its driver passes the suite.

The node cache tier (local disk, HRW placement, peer transfer) stays onggi's own: it decides cold-start time,
whichever backend is underneath.

## Consequences
- The lab can start on its existing SMB share through `fs` and move to MinIO or Ceph later without changing manifests
  or refs.
- Onggi's correctness surface stays small (manifests, refs, cache tier); durability, rebuild and erasure coding are the
  backend's.
- A backend that declares `none` (a single local disk) works for tests and small labs; policy makes the risk visible.
- Throughput differs by backend; the cache tier and prefetch hide most of it, and benchmarks per driver measure it.
