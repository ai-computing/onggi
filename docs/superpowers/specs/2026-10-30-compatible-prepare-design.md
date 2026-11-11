# Compatible artifact preparation

The user authorized the next integration step after storage/delta delivery and
native resident update validation. Implement the first executable part of P3:
choose an explicitly compatible compiled artifact, then prepare its plain files
before a serving process starts. Keep the existing Go CLI/store and add no
dependencies. This is the implementer's scoped first increment, not a claim
that the whole P3/P4 controller, compiler farm or NPU prewarming is complete.

## Approach

Options considered: full Kubernetes controller/CRDs (too many unimplemented
cross-repo contracts); an external catalog file (can drift from artifact bytes);
or optional compatibility metadata inside the content-addressed manifest plus
a CLI consumer. Choose the last: the manifest binds metadata to its files and
existing refs, chunk caches, hash verification and GC leases can be reused.
The user delegated routine design choices and continued execution; implement
inline without another design/permission menu.

Add `Profile *Profile` to Manifest and PushOptions, omitted for legacy pushes.
Profile v1 requires schema, canonical nonzero SHA256 model-source and compile-
options digests, exact device/runtime/SDK/compiler/firmware/format/layout/topology
strings, positive tensor parallelism, batch and sequence. No version ranges,
unknown fields, blank values, shape padding or inferred vendor compatibility.
Publishers are responsible for accurate metadata; this is neither vendor ABI
certification nor a signed attestation. One declared B/S profile per artifact.

`onggi push --profile FILE` validates before opening a backend or publishing.
Existing unprofiled manifests and ordinary push/pull remain compatible.
`onggi prepare --request FILE [--plan-only] [--cache DIR] [--peers URLS]
  [--workers N] --store S [--dir DEST] <ref|digest>...`
resolves each candidate once under a store GC lease, reads hash-verified
manifests, rejects missing/incompatible profiles with field reasons and chooses
the lexically first compatible manifest digest independently of argument order.
Corrupt/unreadable manifests fail closed. Report all candidates and reasons as
JSON; no match returns nonzero and never creates destination/cache. Plan-only
does not fetch model chunks or create destination/cache. Backend opening follows
the existing CLI's store-creation behavior; plan-only is not a globally read-only
backend API. Caller supplies candidates; automatic inventory discovery is later.

Prepare pulls the selected immutable digest through existing verified chunk
cache/peer code into a private sibling temp directory, then renames the completed
tree into a previously absent DEST. A sibling mkdir lock prevents cooperating
preparers from writing the same destination. Do not reuse the running CSI root:
Materializer.Init assumes exclusive ownership and cleans unfinished pulls.
On failure, remove only this call's temp/lock, retain cache chunks, return
nonzero, and never publish a partial ready directory. Existing/symlink/busy
destinations are refused. A crashed process may leave its destination lock;
operators must verify no owner remains before removing it. No serving graph or
device API is invoked. Readiness means verified files on disk, not NPU readiness.

## Acceptance and limits

Tests must catch missing identity fields, wrong versions/options/topology/shapes,
invalid/unknown JSON, legacy manifests, deterministic selection, ref movement,
hash corruption, failure cleanup, existing targets and busy locks, cache reuse
and byte-identical output. Run the Linux suite/race/vet/build and final S3 CI.
NPU execution and latency improvements are not claimed for this metadata/data
path; earlier RNGD/ATOM resident tests retain their own separate scope.
Document reproducible RNGD/ATOM profile examples and an init-container flow.
Do not merge, deploy, change shared SDK/drivers, edit prior provider work or
allocate NPU hardware. Preserve unrelated hub csichk.sh and old worktrees.
