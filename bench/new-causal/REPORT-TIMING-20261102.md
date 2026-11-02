# Repeated prepared-artifact deployment timing

Exact ATOM decode B2_lm→B8_lm, S256:15,238,278,850→15,228,048,882 bytes.
The same frozen POSIX store supplies P (full reconstruction) and R (consume
base). Actual rsync and xdelta3 commands run on the same target. Five blocks
alternate P/R and reverse baseline order after a separate five-arm smoke.
All25 main outputs match the target's complete SHA; no failure/outlier is
excluded. There is no accelerator allocation or compilation.

| Arm | Command median [inclusive IQR], s | Command +sync +full SHA median [IQR], s |
|---|---:|---:|
| P |23.434 [0.312]|31.990 [0.341]|
| R |5.595 [0.346]|14.213 [0.355]|
| rsync |24.047 [0.720]|32.415 [0.861]|
| rsync --inplace |11.252 [0.064]|20.225 [0.392]|
| xdelta3 decode |17.648 [1.048]|27.116 [1.638]|

These are different clocks, each aggregated from complete per-trial records;
do not add marginal medians. Source/input rehash, independent base copying and
output setup precede the command and are separately recorded. Sources are
rehashed before every arm, so this is a warm-cache policy with shared OS cache
and uncontrolled colocated load. Application chunk cache is disabled; no peers.
It is local POSIX delivery, not a network/S3 or NPU-ready comparison.

rsync uses --no-whole-file --ignore-times --stats in both arms; only --inplace
differs. Matching is inside its command. xdelta encoding is done once before
the repeated decode trials; the cohort records its separate preparation time,
delta bytes and SHA. Neither its encoding nor Onggi publication is charged to
deployment. No end-to-end publication-inclusive superiority is inferred.

The primary policy is the existing file-kind two-level policy,256KiB frames,
auto compression, workers8, uploads4, GOMAXPROCS32 and nice19. CPU requests4,
limit12; memory request8Gi, limit32Gi. Actual submitted/observed Jobs and
cgroup quota witnesses are retained. Their SYS_PTRACE capability supports the
separate diagnostic; these containers are not privileged. All three CPU
preparation/smoke/main Jobs were UID-cleaned with Pods absent.

Source runnerc2288e7 / SHA15468016...554f5a, Onggi core05c9760 / binary
SHAe6b8ee52...15ae5. Full identities are in frozen records. The package has
64 smoke and184 main worker files plus43 exact operational records. Exact
compressed exports, stdout/stderr, executed arguments, GNU time, input hashes,
independent inode witnesses, output hashes and cleanup receipts are retained.
Large model payloads/binary/delta files are absent. Offline checks validate
worker-recorded hash witnesses; they cannot rehash absent models.

Reproduce offline checks from the repository root:

```sh
python3 -m unittest discover -s bench/new-causal -p 'test*.py' -v
python3 bench/new-causal/verify_timing.py timing-smoke
python3 bench/new-causal/verify_timing.py timing-main
```

The checker reproduces all aggregates and validates raw clocks, exact archives,
same frozen source/target, full-output hash witnesses, real R admission and CPU
UID cleanup. A mutation test resigns an entire export with a false validated
clock and still requires rejection. Model experiments use the frozen runner;
the added offline checker does not change that measured source.

Pre-publication review found three missing verification boundaries, not an
observed invalid trial: GNU-time agreement, complete expected method arguments
and small build/store provenance. Regressions first showed resigned false
command clocks and wrong stores could pass; checks now bind full expected
argv, actual raw GNU-time/resources and independently timed CLI duration. The
GNU-time consistency check allows up to1s of outer launch/recording overhead
and0.02s rounding; it never subtracts or adjusts a measured value.

Supplemental exact records supply the actual350-byte store config, preparation
script, original shared-store push commands/output, source archive, Go1.25.1
and binary-hash/build-status receipts. Code-only canonical LF comparison binds
the archived cmd/pkg/module files to core05c9760; raw records stay byte-exact.
The CLI reports dev because the source archive has no Git checkout. The
earlier CPU-only Go build used a privileged container; its submitted image,
commands and successful UID cleanup are separate from the three nonprivileged
preparation/timing Jobs. The native/core and measured runner are unchanged.

Logical local writes/read bytes are **unavailable in this untraced timing
cohort**. CLI stats.bytes is target size, and GNU time filesystem blocks are
not physical device/NIC/DMA bytes. The separate same-target-cache traced P/R
cohort must pass its own gates before an I/O reduction is claimed. Matched
publication C/F/T/TF/TA data remain a separate study.
