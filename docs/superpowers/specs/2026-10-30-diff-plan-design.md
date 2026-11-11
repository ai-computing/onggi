# Reuse the target diff plan

The user authorized continued Onggi/NPU loading optimization. CPU profiling of
the immutable 8B fixtures isolates 333–365 ms in the live Onggi CLI comparison
versus 6–13 ms in Python row planning. Store.Diff currently builds the target
plan twice: once via Extents, once to count distinct new stored bytes.

Reuse the same target nodes for extents and NewStored. Keep the base plan,
validation, GC/repack fallback, range order, JSON/API, and trust checks intact.
This is local to one Diff call: no persistent cache or stale-ref semantics.

Alternatives: cache diff JSON across calls (larger invalidation/trust surface),
or optimize Python headers (small measured share). Our call is to remove the
redundant native store plan first. The user's go-ahead continues autonomous
implementation; no additional design approval is needed.

Acceptance: locator read once per target; legacy index, stale locator/repack,
corrupt metadata and cancellation behavior preserved; results unchanged on
same/moved/new and real 8B sparse/LM fixtures. Compare interleaved baseline and
candidate binaries built with one toolchain. Run race/vet and S3 CI. This does
not change NPU execution and does not claim a cold-load improvement.
