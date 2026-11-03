# Prepared literal-range patch reference (issue95)

This benchmark supplies a deliberately simple, sound composition: a publisher
precomputes target literals, an exclusive private base retains same-offset
bytes, and a correct verified target is published. It is a comparison control,
not a production patch format, ownership service, optimized codec or new
matching algorithm. **Real-model measurement: not_run.**

Python3.10+ stdlib and regular files on one filesystem are sufficient. The
filesystem must support hard links for exclusive target publication. No SDK,
accelerator, cluster, network or installation is involved in these local tests:

```sh
python3 -B -m unittest discover -s bench/range-patch -v
```

## Representation and operations

An externally supplied keep plan is a sorted JSON array of `[offset,length]`
pairs. These are same-offset byte ranges, with positive lengths and no overlap.
It can be derived from existing artifact metadata; the reference does not
discover matching ranges itself. Preparation actually compares each declared
keep range in the base and target. Remaining target bytes become consecutive
raw literals in `payload.bin`; `manifest.json` records original/target sizes
and SHA256s, keep ranges, literal offset/length/payload location/digest and the
complete payload digest. Every target byte is either kept or a literal.

```sh
python3 -B bench/range-patch/literal_patch.py prepare \
  --base /private/input/base --target /private/input/target \
  --keep /private/input/keep.json --output /private/NEW-PATCH
python3 -B bench/range-patch/literal_patch.py apply \
  --base /private/verified-base --patch /private/NEW-PATCH \
  --manifest-sha256 PUBLISHED_MANIFEST_SHA256 \
  --exclusive --output /private/NEW-TARGET
```

The manifest SHA comes from the actual prepare receipt and must be retained in
the independently frozen application config, like a published artifact root.
Do not recover it from a possibly changed manifest at application time.

Preparation reads/hashes full source files, verifies every keep interval,
writes/synchronizes payload then writes/synchronizes the completion manifest.
It exposes source reads, raw literal/total patch bytes and elapsed time.
The input files and preparation paths must be immutable/private. Failed
preparation is retained without a completion manifest; existing paths are
never overwritten. A1-TiB file and64-MiB manifest limit bound this reference.

Application requires the caller to own the base and output parent exclusively;
`--exclusive` declares that precondition, it does not discover leases. Symlink
files, hard-linked bases and cross-filesystem applications are refused. The
patch must remain immutable. Before mutation the reference verifies the pinned
manifest, complete payload/literals and complete base identity. It then
withdraws the base into a private work file, writes raw literals, truncates,
fsyncs and hashes the complete target. An exclusive hard link publishes the
verified file, followed by removal of the work name. It never overwrites an
existing or racing output. Before-commit errors retain any work file without
publishing a new target; rollback is not promised. An unlink error after
successful publication reports the committed target plus cleanup error and
retained work path. Directory crash durability is not claimed.

## Prepare from the measured core's public diff

`diff_keep.py` connects the existing **legacy FileDiff** output (the measured
core's `onggi diff --ranges --json`) to this reference. It does not parse opaque
model files or implement matching. Freeze the actual diff bytes and complete
base/target SHA256s independently before invoking it:

```sh
python3 -B bench/range-patch/diff_keep.py \
  --base /immutable/base.bin --target /immutable/target.bin \
  --base-path decode_S256_B2_lm.rbln --target-path decode_S256_B8_lm.rbln \
  --diff /frozen/diff.json --diff-sha256 FROZEN_DIFF_SHA256 \
  --base-sha256 FROZEN_BASE_SHA256 --target-sha256 FROZEN_TARGET_SHA256 \
  --output /private/NEW-PATCH
```

The artifact paths are exact selected `files[].base/path` names, independent
of physical source filenames. The adapter refuses another pair, duplicate
paths, missing changed ranges, inconsistent counters, invalid coverage and
keeps beyond the actual base size. It complements changed ranges, verifies
complete input pins, then lets the reference compare every kept byte. Empty,
grown, shrunk, moved-only and identical files have explicit handling. Newer
switchplan schemas are deliberately refused, not silently interpreted.

The NEW package includes `keep.json` and `preparation.json`. The receipt binds
diff/keep/input/source SHA256s, patch manifest identity and measured preparation
costs. `preparation_total_seconds` includes metadata processing, extra full
input verification, patch construction and keep-file writing; it ends before
receipt serialization/write. That extra input verification is also counted
separately and must be charged. The diff's original publication cost and its
producer/binary/config identity still need separate frozen records: this
adapter cannot infer them from a diff hash. The patch manifest remains the
application pin; do not use metadata alone as byte-equality evidence.

Linux CI additionally sets `ONGGI_TEST_BIN` to test the actual Go push/diff →
adapter → patch application path on synthetic opaque bytes. Without this env,
that one integration test is skipped; all Python real-file tests still run.
No model cohort, online comparative timing or device operation is run here.
The prepared-patch step also runs after unrelated test failures. A separate
synthetic causal diagnostic retains its raw traces in the workflow artifact;
this does not override an earlier failure or relax the trace parser.

## Fair comparison and evidence limits

`cohort.py` implements the common-readiness R/L executor comparison. Freeze an
explicit config with complete input identities, actual complete target cache,
prepared patch root, driver SHA, external build receipt pins and source revision.
`COHORT-PLAN.md` gives its fixed conditions. Model mode admits only the
preregistered complete decode batch-2→batch-8 pair on Linux; fixtures remain
labeled synthetic. Independent input/build/host admission is still required.

```sh
python3 -B bench/range-patch/cohort.py freeze --config CONFIG --output NEW-FROZEN
python3 -B bench/range-patch/cohort.py run --frozen NEW-FROZEN --output NEW-SMOKE --stage smoke
python3 -B bench/range-patch/cohort.py run --frozen NEW-FROZEN --output NEW-MAIN --stage main \
  --smoke-path NEW-SMOKE/cohort.json --smoke-sha256 INDEPENDENT_SMOKE_SHA256
```

An attempt copies/fsyncs a private base and its method's local seed, then starts
the online clock before integrity work. R verifies the base, invokes the actual
admitted materializer and syncs/full-hashes its private output. L invokes the
actual reference CLI with internal base/payload/full-target checks. Both prove
donor inode transfer before exclusive consumer publication. A separate complete
SHA audit follows the common endpoint. Python vs Go initialization and each
method's actual validation work remain charged; no executor-only attribution
is implied. Command timeouts kill the direct single-process method; a model Job
also needs the outer active deadline for preparation and verification.

Excluded smoke is RL; main has five paired blocks RL/LR/RL/LR/RL. Main rechecks
the pinned smoke's actual method/identity/clock witnesses. Failures stop and
retain work without retry or valid-subset aggregation. A frozen successful-data
cleanup choice removes only known new private trial children after audit while
retaining raw results/stdout/stderr. Method cleanup and audit are separate from
online readiness. Logical retained path bytes count stage/ready hard-link
aliases twice and are explicitly not physical occupancy; use the separate
owner-sequence experiment for retained-state economics.

Linux CI builds the existing three-arm driver and runs actual synthetic R/L
smoke and five paired blocks. This validates the runner, not model latency.

- A valid model cohort still needs pinned source/input/keep-plan/patch/config,
  actual base ownership and identity, an excluded smoke, complete attempt
  outcomes and independently verified repeats. Local fixture durations are
  not model performance. Freeze admission before observing timing.
- The reference receives precomputed ranges/literals. Charge that publication,
  additional storage and retention; do not call its online application a
  preparation-inclusive result. Raw literals do not share the product's
  compression/frame encoding. This is a direct executor control, not a
  codec-matched storage superiority test or a complete xdelta/rsync replacement.
- Application includes full base verification, patch verification, target
  writes/sync and complete target SHA. A compared materializer must receive
  the same integrity/completion contract and timing boundary. Do not compare
  this total with another method's command-only time and attribute the gap
  solely to its executor. Exposed phase times do not excuse unequal contracts.
- Counters describe logical Python file reads/writes. They exclude filesystem
  readahead, writeback, physical I/O and network framing. Metadata I/O is not
  included in payload counters. Use a separately validated tracer for syscall
  attribution. Failed applications retain actual state and recovery cost.
- A sound range patch already provides positional retention under owner
  permission. Any broader benefit from a family store, base discovery or
  distribution needs separate experiments against this feasible composition.

Fixtures cover edits, shifted/reordered targets, growth/shrink/identity/empty
files, multi-buffer literals, wrong/corrupt/truncated input, truthful ownership
refusal, path aliases, output races and errors before/after commit. Passing
them does not establish real-model latency, lifecycle benefit or concurrency
beyond these forced schedules.
