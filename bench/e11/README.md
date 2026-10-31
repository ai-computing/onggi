# E11: simultaneous file-only variant transition

This is a frozen lab experiment for [issue82](https://github.com/ai-computing/onggi/issues/82),
not a new runtime or a synthetic networking benchmark. Read [PLAN.md](PLAN.md)
for the fixed treatment/cache conditions and retained setup attempts.

## Live execution

Only `cpu-1`, `cpu-2`, `etri-001` were eligible in the 2026-10-31 cluster.
The origin is colocated with the etri recipient, and has an active 1GbE NIC.
These conditions limit extrapolation to other topologies. No nodes were
uncordoned, taints bypassed, or accelerator resources requested.

All remote commands from the Windows research hub use `hub.orca.run_remote`
through the existing dell_0/rebellion worktrees. Do not substitute SSH.
`deploy.py` runs on dell_0 using its live kubeconfig. It creates only the
dedicated `codex-e11-20261031` namespace and bounded `cdx-e11-*` CPU-only Jobs.
The origin's source store hostPath is mounted read-only. Its writable path is
the owned `/home/gspark/workspace/codex_e11_20261031/attempt03` on etri-001;
each worker uses an isolated bounded emptyDir. Origin UID/GID1002 matches the
prepared host directory; worker UID/GID1000 uses its emptyDir group.

Prerequisites are the exact existing E6 complete artifacts/store on etri-001,
the main05c9760 CLI, CI-pinned VersityGW1.8.0 and the recorded Python image.
Full artifact payloads/binaries are not shipped in Git. `prepare_inputs.py`
rehashes the source inputs without allocating a device. Input and tool
hashes, public source identity, setup logs, submitted Jobs and actual Pod
assignments are retained in the raw package. The disposable S3 fixture
identity is the same public test fixture used in CI, not a production secret.

The controller stages these exact scripts via a ConfigMap. After all three
bootstrap registrations, the commands are:

```sh
python3 run_matrix.py --stage smoke  # four cohorts, never included in n5
python3 run_matrix.py --stage main   # 20 cohorts, 60 node samples
```

Each cohort prepares independent base-copy inodes and fresh caches, then
releases all workers through a central barrier. Store and peer arms have
identical initial cache placement. The one-seed case includes node0's local
warm cache in both arms; seeding is outside the barrier and reported separately.
The empty peer snapshot cannot forward newly arriving chunks during that pull.
All reconstructed files must equal the pinned target SHA inventory. Logical
node-cache coldness does not imply cold OS page cache.

Stop workers through the origin `/stop` endpoint after the matrix, wait for
their Jobs to succeed, then `/shutdown` the origin. Preserve terminal APIs,
logs and original response records before UID-guarded Job deletion. Never
delete unrelated workloads or modify the original artifact/store paths.

## Independent verification

Published results: [report](REPORT-E11-20261031.md),
[verified summary](summary-20261031.json), and [figure](figures/e11-cohort.pdf).
The complete raw package is [data/20261031](data/20261031), preserved by tag
`ccgrid-e11-20261031-raw-v1`. Python 3.9+ is required for offline analysis.

```sh
python3 -m unittest discover -s bench/e11 -p test_e11.py -v
python3 bench/e11/analyze.py --root bench/e11/data/20261031 --out reproduced-summary.json
diff -u bench/e11/summary-20261031.json reproduced-summary.json
# Optional figure; requires Matplotlib only on the analysis machine:
python3 bench/e11/plot.py reproduced-summary.json --out figures
```

The analyzer rehashes the package, checks exact target and ready-cache
inventories, recomputes origin response counters from original request rows,
requires controller/origin cohort copies to agree, verifies output hashes and
cleanup acknowledgements, and aggregates all four n5 groups with inclusive
quartiles. Any failed main cohort is an error, never a latency exclusion.
The worker hashes actual complete output files during every run. Artifact
payloads are absent from Git: the offline verifier compares those recorded
hashes with the frozen input inventory; it does not rehash model payloads.

`read_bytes` counts stored-form range payload from the origin; `peer_bytes`
and `cache_bytes` count their respective stored chunk payloads and can use
different compression/framing. The origin proxy additionally records metadata
and upstream body bytes before writes; interrupted responses invalidate a
cohort. These values are not physical NIC, TCP, DMA, disk-I/O or billing counters.
The cohort clock ends at the last pull-completion marker received centrally;
full-file hash validation and cleanup durations are separate. NIC counters
include unrelated traffic. The experiment measures no inference or CSI mount
latency and provides no vendor-versus-vendor performance comparison.
