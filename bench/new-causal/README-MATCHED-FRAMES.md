# Matched frame controls (issue90)

Status: **source/reference build under review; no new model cohort run**. Historical causal.py,
its measured source identity and all earlier raw results are unchanged.

The missing control is flat fine CDC with the same encoding opportunity as
the hierarchy, including the full19-file/263893021566B ATOM set. Setting
`--sub 0 --frame 256KiB` does not enable frames: current CLI/store code only
constructs frames for selected sub-chunked files. This experiment therefore
uses four explicit policies, all zstd/64MiB packs/eight workers/four uploads:

| Policy | Top-level min/avg/max | Sub-split min/avg/max | Frame |
|---|---|---|---|
| D |1/4/16KiB|disabled|off|
| I |1/4/16KiB|64/256/1024KiB identity|off|
| F |1/4/16KiB|64/256/1024KiB identity|256KiB|
| H |16/64/256KiB|1/4/16KiB|256KiB|

The production store rejects this identity setting because its sub-chunk
maximum exceeds the top-level minimum. The first draft's real CI fixture
failed at I; this was an implementation failure, not a model result. The
reference builder now copies pinned1e3c74f Git source into a separate fresh
tree and changes exactly one guard in that copy:

```go
// Product:
if c.Sub.Max > p.Min {
// Experimental reference:
if c.Sub.Max > p.Min && c.Sub.Min < p.Max {
```

This admits only the additional case where the minimum sub-size is at least
the maximum parent chunk, so every sub-split is an identity. All non-identity
oversized sub-chunkers remain rejected. No product source file is edited;
the executed reference is **a modified experimental build**, not an existing
unchanged-core capability. Its manifest preserves every original/reference
source hash, the exact one-line patch, generator hash, Go/check commands and
binary hash. It runs Go vet/race tests before building. All four policies
use this same reference binary; F/H processing code is otherwise unchanged.
Freeze validates the pinned base, reviewed generator hash, canonical identities
of all67original/reference files, the exact guard patch, successful command
arguments and hashes of all eight retained build/check log files. The runner
directly compiles checked generator bytes and archives its source and logs.
These are consistency/provenance checks, not a cryptographic attestation of
remote execution; independent CI/node control receipts remain required.

The selected kinds are atom-prefill/atom-decode/atom-other in every policy
where sub-splitting is enabled. For I/F the minimum sub-size exceeds every
top-level chunk, so each split returns the whole fine chunk. The inspected
one-child-equals-parent path emits no composite. I/F consequently have the
same fine per-file lists as D while using the existing frame code in F.
The actual multi-file CLI fixture must prove this, zero flat composites and
real framed entries; flags alone do not establish the treatment. Framing can
legitimately choose independent compression when it is smaller, so a model
cohort need not produce frames for every entry.

D/I exposes identity-adapter hashing/dispatch overhead. F/H is the matched
encoding-opportunity storage comparison. F is **not** a new optimized native
flat-framing implementation; publication time retains that caveat. Counts
include finalized pack payload and all metadata. Bulk proprietary model/output
bytes and bulk indexes are not implied independently available offline.
Index counts are worker-counted; raw index headers/first-frame entry witnesses,
object hashes and lengths are preserved. A selected raw witness is not an
independent recount of absent complete indexes.

Local source checks:

```sh
python3 -m unittest discover -s bench/new-causal -p test_matched_frames.py -v
ONGGI_TEST_BIN=/absolute/unmodified-onggi python3 -m unittest discover -s bench/new-causal -p test_matched_frames.py -v
python3 bench/new-causal/flat_frame_reference.py --repo /absolute/product-checkout --output /owned/new-reference
python3 bench/new-causal/matched_frames.py fixture --onggi /owned/new-reference/onggi --output /owned/new-fixture
```

The explicit CLI fixture is correctness-only synthetic data, not model
performance. Existing CI builds a product CLI and discovers all tests. The
new test first checks its expected guard refusal, then builds the separate
reference and verifies its four-policy roundtrip/list/frame witnesses. The
reference source comes from Git blobs, not untracked local files or cached
Python bytecode. Linux/GNUtime/nice/Git/Go and the supplied binary are required; nothing installs tools,
contacts a remote host or allocates a device.

For a model cohort, create an external `status=not_run` configuration declaring
`dataset_kind=model`, source_dir, the complete sorted input_inventory
(path,bytes,sha256), onggi path/binary_sha256, core_revision,
binary_build_provenance, limits_description, workers8, gomaxprocs32 and a
positive free_margin_bytes, and reference_manifest_path for the built experimental
reference receipt. Bind the actual historical family before freezing;
count/size checks cannot establish that an arbitrary inventory is that family.
Caller preflight must separately prove core/build provenance and resources.

```sh
python3 bench/new-causal/matched_frames.py freeze --config /owned/config.json --output /owned/frozen.json
python3 bench/new-causal/matched_frames.py run --frozen /owned/frozen.json --output /owned/smoke --stage smoke
python3 bench/new-causal/matched_frames.py run --frozen /owned/frozen.json --output /owned/main --stage main --smoke /owned/smoke/cohort.json
```

Smoke has one DIFH block. Main has five rotated DIFH/IFHD/FHDI/HDIF/DIFH
blocks, five observations per policy. The complete matching smoke must pass
before main. Each attempt rechecks immutable source/binary/config identities,
uses a fresh store, validates its actual configuration and object inventory,
pulls the entire artifact and verifies full input/output hashes. Successful
store/output trees are deleted only inside the owned trial after raw records
are saved; failed trees remain and stop the dependent cohort without retry.
Sources are warmed by these rehashes; OS cache/load are uncontrolled.

Publication is the push command; full-store inspection and roundtrip validation
are separate clocks. GNUtime CPU/RSS fields remain unavailable if absent, never
zero-filled. Exact raw export/replay and separate build/control provenance are
required before model results enter a paper. Current issue88 measurements and
their export have sole use of the node/transport; this runner is not queued to
overlap them.
