# CPU causal cohorts (issue #84)

Real model experiments: **not_run**. This directory supplies a runner and local
CPU/file tests. It does not change runtime code, allocate a device, compile a
model, contact a node, or modify manuscript evidence.

The frozen protocol is the 2026-11-02 CPU causal ablation note. Primary inputs
are the exact original-named `decode_S256_B2_lm.rbln` (15,238,278,850 B) and
`decode_S256_B8_lm.rbln` (15,228,048,882 B), one regular file per private input
directory. The example's binary SHA identifies the historical main05c9760
binary, not a newly verified local file. Fill provenance with the actual binary
receipt/build and Go version. A rebuilt binary needs its own SHA. Preserve the
original source files and historical stores. Remote commands must be launched
by the hub's `run_remote`; this script runs only on the chosen CPU host.

## Prerequisites and local checks

Python 3.10+, Linux, the pinned Onggi CLI, GNU `time`, `nice`, and sufficient
scratch space on the declared filesystem are required. The optional diagnostic
requires working `strace -ff -yy`/ptrace permission. `rsync` arms require the
actual installed rsync with `--inplace`. Add `xdelta` to `deployment_arms` only
when actual `xdelta3` is available. Nothing installs tools or changes host
settings. Record real CPU/memory limits in the config; the runner fixes
`GOMAXPROCS=32`, `--workers 8`, `--uploads 4` and nice level 19, and records
affinity, cgroup limits, mounts, load and free space. It does not impose a new
cgroup or memory quota.

```sh
python3 -m unittest discover -s bench/new-causal -p test_causal.py -v
ONGGI_TEST_BIN=/absolute/pinned/onggi python3 -m unittest discover -s bench/new-causal -p test_causal.py -v
python3 bench/new-causal/causal.py fixture --onggi /absolute/pinned/onggi --output /owned/new-fixture
```

The first command tests real local file copies and Linux syscall accounting;
the actual CLI fixture skips unless `ONGGI_TEST_BIN` is supplied. The fixture
uses deterministic 8-MiB data with duplicate placements, a 64-KiB interior revision, one shared store,
actual plain/consumed pulls and full-output SHA checks. It checks full writes
against retained bytes using real traced syscalls. It repeats P/R with fresh
copies of one A-seeded cache, including B's cache misses and staging writes.
It is test data, never a
model result. Tests also cover duplicate placements, moved/short reads,
vector/concurrent writes, error returns, cache reads, unsupported mmap,
incomplete traces, pair ordering, metadata accounting and preserved failures.

Local Windows/WSL verification ran all available Python tests, including the
real WSL strace fixture. The actual Onggi CLI fixture and Go suite could not run
locally because no pinned Onggi binary or Go installation was present. Before
real-node smoke, run the second command above and the repository check
`go test -race ./...` using the existing CPU environment. No system installation
is authorized by these instructions.

## Freeze and run

Copy `config.example.json` outside the checkout, fill the actual paths,
provenance and limits, and choose the arms before freezing. For deployment,
set `store` to one existing frozen POSIX store and supply its exact
`sha256:...` base/target manifest digests. The freeze command hashes the
binary, every immutable input, and every store/cache object; it writes a new
`status=not_run` record. Never republish separately for P and R. For publication
only, `store` and both manifest values may remain null.

```sh
python3 bench/new-causal/causal.py freeze --config /owned/conditions.json --output /owned/frozen.json
python3 bench/new-causal/causal.py run --frozen /owned/frozen.json --experiment deploy --stage smoke --output /owned/deploy-smoke
python3 bench/new-causal/causal.py run --frozen /owned/frozen.json --experiment deploy --stage main --smoke /owned/deploy-smoke/cohort.json --output /owned/deploy-main
python3 bench/new-causal/causal.py freeze --config /owned/io-conditions.json --output /owned/io-frozen.json
python3 bench/new-causal/causal.py run --frozen /owned/io-frozen.json --experiment deploy --trace --stage smoke --output /owned/io-smoke
python3 bench/new-causal/causal.py run --frozen /owned/io-frozen.json --experiment deploy --trace --stage main --smoke /owned/io-smoke/cohort.json --output /owned/io-main
python3 bench/new-causal/causal.py run --frozen /owned/frozen.json --experiment publish --stage smoke --output /owned/publish-smoke
python3 bench/new-causal/causal.py run --frozen /owned/frozen.json --experiment publish --stage main --smoke /owned/publish-smoke/cohort.json --output /owned/publish-main
```

For the primary I/O diagnostic, `io-conditions.json` preserves the exact same
binary/store/input/cache/limits but declares only `deployment_arms: ["P", "R"]`.
Baseline diagnostics use separate conditions/smokes: for example rsync may use
file-backed source mmap, whose read bytes are unavailable to this parser. Such
a baseline diagnostic stops without making a logical-I/O claim; it does not
invalidate the separate untraced timing experiment.

Every output directory must be new and disjoint from immutable inputs,
store and cache. Smoke is one excluded repetition per declared arm; main is
five repetitions. Main requires a successful smoke with the identical frozen
config, arms, experiment and trace policy. P/R alternate PR/RP/PR/RP/PR even
when baseline arms are present; baseline order reverses in alternate blocks.
Publication rotates C/F/T/TF/TA order. No failure or fallback is automatically
retried or discarded; the dependent matrix stops, preserves all files, and
leaves the missing repetitions visible. A retry needs a new directory and an
external link to the prior attempt.

Before each arm, the runner rehashes both immutable source trees to establish
the warm page-cache policy. It never drops host caches. Default application
chunk cache is disabled and there are no peers. If `cache_dir` is explicitly
set, both P and R receive fresh verified copies of the same frozen cache;
these observations form a separate stratum. Background host/page-cache
interference remains possible.

Both P and R start with an independent verified writable base copy; only R
offers it. Every file must have a different `(device,inode)` from the immutable
source and link count one. P starts without an output directory. R is valid
only when actual JSON `stats.base` equals the frozen base digest. Refusal,
local-read fallback or wrong output SHA remains an invalid intended treatment.
For the rsync arms, the copied B2 bytes are renamed to the B8 destination name
before the timer; no serving reader may access that idle destination. The two
commands differ only in `--inplace`:

```sh
rsync --no-whole-file --ignore-times --stats SOURCE_B8 INDEPENDENT_DEST/
rsync --no-whole-file --ignore-times --stats --inplace SOURCE_B8 INDEPENDENT_DEST/
```

These are local sender/receiver transfers, not WAN measurements. Sent and
received protocol bytes are both retained and summed. There is no compression
in these arms. Compressed variants need a separately frozen cohort.

With `xdelta` declared, actual `xdelta3 -e -s BASE TARGET delta.vcdiff` encoding
runs once as a separately logged preparation/publication stage before the
deployment trials. Each decode runs `xdelta3 -d -s INDEPENDENT_BASE delta.vcdiff
OUTPUT`. Encoded bytes/hash and encode timing are retained. There is no
synthetic xdelta result or substitute implementation when the tool is absent.

## Clocks, counters and artifacts

Each deployment trial records preparation/copy proof, command argv and UTC /
monotonic timestamps, common final file `fsync`, and identical full-output
SHA/list validation separately. `command.elapsed_s` is process launch through
exit; `durable_s` runs from that same start through final file sync;
`validated_s` runs through the common SHA check. Preparation-inclusive elapsed
time starts before immutable-source warming/copy and ends at the same SHA
boundary. Parent-directory crash durability and service readiness are outside
this CPU experiment. GNU time user/system CPU, maximum RSS in KiB, and
filesystem block counts remain separately named. RSS is the maximum reported
for that process tree, not summed simultaneous memory use.

**CLI `stats.bytes` is target size, never observed target writes.** Untraced
timing trials leave `logical_io` unavailable. Separate traced cohorts run all
threads/children with descriptor path annotations and count successful return
lengths for regular/vector/positional reads/writes and file-copy syscalls,
with output/base/store/cache, immutable-source, delta-file and private staging
attribution. Method commands use per-trial `TMPDIR`; system/program/logging I/O
outside these named artifact roots is excluded. `ftruncate`/sync calls are recorded
separately and never converted to written payload. These are logical syscall
bytes during the command, not physical disk bytes. Full-output validation is
outside that trace. File-backed mmap, io_uring, unrecognized successful file ioctls, incomplete or
unparseable traces invalidate the diagnostic; missing counters stay null.
Traced latencies must not be pooled with or compared against untraced ones.
Process-wide `/proc/PID/io` stays null: no validated pre-exit final snapshot is
implemented. Neither warm-cache zeros nor size-minus-reused arithmetic fills it.

Publication creates a fresh store for every arm/repetition and pushes A then
B using identical zstd policy, 64-MiB packs, worker/upload limits and scratch
policy. C uses 16/64/256-KiB CDC, F uses 1/4/16-KiB CDC, and T uses coarse CDC
plus 4-KiB subchunks; **all three use frame 0**. TF alone enables 256-KiB frames;
TA also switches codec mode to auto. The actual stored config, object inventory
after A/B, pack/non-pack metadata bytes, finalized totals, B-only deltas,
temporary bytes, `du -sb` apparent and `du -sB1` allocated output are retained.
`push_b.elapsed_s` is B-only publication; `family_publication_s` is the sum of
both push intervals in that trial. Inspection/diff and roundtrip validation
are outside push timers. Both artifacts roundtrip with SHA/list equality.
Rsync publication is not applicable because the raw source already exists;
xdelta encoding is a separate preparation clock. No sum of marginal medians
is a publication-plus-deployment measurement. The two-file result cannot close
a full 19-file causal storage claim.

Each trial keeps stdout/stderr, raw GNU time/trace files, command records,
copy/inode evidence, output SHA inventory, result and cleanup receipt.
`cohort.json` keeps every sample and derives medians plus inclusive Q1/Q3 only
for complete valid cohorts. All failed/invalid samples remain visible; a
failed/incomplete cohort receives no valid-subset aggregate.

By default successful large artifact trees are deleted only after validation;
raw evidence and inventories stay. Set `retain_outputs=true` before freeze to
retain them. Failures retain their trees. Cleanup validates resolved targets
inside that trial and emits a receipt. Ensure capacity for source files,
frozen store, one independent base/output/cache and trace logs at minimum;
retention across all arms needs much more. Recorded artifact byte snapshots
and per-file allocated blocks do not establish an observed peak or unique
physical extent usage. Peak scratch/RSS/PSS/device-memory and process-I/O
claims remain unavailable unless independently collected.
