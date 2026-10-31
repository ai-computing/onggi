# E11 file-only cohort experiment

User request: CCGrid collaborator, 2026-10-31 10:14 KST; issue #82.
No NPU/GPU requests, device mounts, inference, node configuration changes, or
edits to the paper numbers ledger. Runtime source is main `05c9760`.

Three eligible workers (`cpu-1`, `cpu-2`, `etri-001`) switch the existing complete
RNGD B4/S256/g2 artifact to B4/S512/g2 concurrently. These are opaque files;
their already compiled graph/checkpoint SHA inventories define correctness.
Other workers are unreachable, cordoned, or under disk pressure. Record this
selection and heterogeneous NIC/network limits rather than bypassing them.

Use the existing CLI S3 backend and `onggi peer serve`, without changing runtime
code. A bounded CPU-only origin Job on etri-001 serves the existing immutable
store through the same pinned VersityGW release used in repository CI. A
transparent HTTP measurement proxy records actual successful response body
bytes by object kind and cohort. One recipient is colocated with the origin;
report this explicitly. Instrumentation overhead applies equally to both arms.

Compare store-only and peers with the same cache placement in each stratum:

* `empty`: recipients have no target chunks; the peer cache starts empty.
* `one-seed`: node0 has only the target chunks required beyond the base, seeded
  before the timed barrier. Other nodes lack these chunks. Node0 has the same
  warm local cache in BOTH arms. Measure seeding separately, including bytes.

Recipients first have verified base files. Each consumable trial base uses
independent inodes (`cp --reflink=auto` when supported, otherwise full copy),
matching the artifact's immutable-source policy. No hardlinks to bootstrap or
original experiment files. Preparation/copy time is outside the timed barrier.
Each trial has a fresh cache/destination and its own directory. Page cache is
uncontrolled and no cold-RAM claim is made. Peer setup/index readiness and
artifact preparation occur before the barrier and are separately reported.

One two-arm smoke per stratum precedes the fixed matrix: five cohort trials
per arm per stratum, alternating arm order by repetition. Failures are retained;
smokes are excluded from summaries. No retry or exclusion based on latency.

All workers acknowledge ready before a central HTTP barrier releases them.
Record per-node monotonic CLI wall time, central received-start skew, central
release-to-last-completion, and output hash-validation time separately. Output
hash validation follows the timed pull. Failed hashes invalidate the cohort.
No reliance on synchronized host wall clocks for latency subtraction.

Report CLI `read_bytes`/`peer_bytes`/`cache_bytes`, origin HTTP pack/metadata
body bytes and requests, seed bytes, n, median and inclusive IQR. These are
application payload measures, not physical NIC, TCP, DMA, or disk-read bytes.
NIC counter deltas include unrelated traffic and are contextual only.

## Execution ledger

- [ ] Pin source, graph/file inventories and tool hashes; inspect NICs.
- [ ] Implement the small CPU-only controller, origin accounting and worker.
- [ ] Assert/test accounting, barrier/failed-node aggregation, hash checks.
- [ ] Commit/freeze harness before smoke and main; preserve every attempt.
- [ ] Complete four smoke cohorts, then twenty main cohorts (60 node samples).
- [ ] Recompute all summaries from raw; independent review; green/read CI;
      PR/merge and shared board milestones. Clean owned Jobs/Pods by UID.
