# Exact evidence for the separately frozen three-arm cohort

These tools start no model measurements, remote commands, devices or system
installation. New model numbers remain unaccepted until the corresponding raw
archives and separate build/Job-control receipts have been checked. Historical
two-arm timing/I/O/publication data and measured source1e99504 stay unchanged.

`three_arm_evidence.py pack` runs only after measurement has stopped. It writes
exact raw/source records into bounded48MB tar.gz parts, with per-record,
per-part and whole-archive SHA256. Input/output/store/cache payload trees are
excluded. Extraction requires a fresh directory and checks every declared file;
archive traversal, omitted/duplicate records and altered parts are rejected.

```sh
python3 bench/new-causal/three_arm_evidence.py pack \
  --source "$three_arm_run_root" --label smoke --output "$three_arm_export_root/smoke"
python3 bench/new-causal/three_arm_evidence.py pack \
  --source "$three_arm_run_root" --label main --output "$three_arm_export_root/main"
python3 bench/new-causal/three_arm_evidence.py verify \
  --root "$three_arm_export_root/smoke" --label smoke
python3 bench/new-causal/three_arm_evidence.py verify \
  --root "$three_arm_export_root/main" --label main --smoke-root "$three_arm_export_root/smoke"
```

Use io-smoke/io-main for the separately traced model cohorts. Do not pool their
timings with untraced observations. Main acceptance requires its exact matching
excluded smoke archive and identical frozen identity. Every planned attempt
must remain present; invalid records do not yield a valid-subset result.

The original synthetic integration fixture has a **traced `smoke/` directory**,
without an io-prefix. Verify that archive with `--fixture --label smoke`.
Its literal synthetic source revision identifies this layout; model label and
frozen-input gates remain strict. Fixture reports always say model_evidence=false.
The additional synthetic unit fixtures exercise gates, never model performance.

Replay directly compiles the fixed hash-checked archived runner/helper bytes,
bypassing disk bytecode. It compares exact raw JSON/stdout/GNUtime, full-target
hash witnesses, independent donor/cache copy identities, actual branch counters
and donor outcomes. It recomputes command/sync/target-validation/diagnostic clocks
from retained timestamps and verifies raw GNUtime consistency. Every recorded
environment whitelist field, including GOMEMLIMIT=None, must match the protocol.
Traced reports reparse every thread file and require C donor reads to cover its
actual local_bytes; P/R must have no donor reads. Raw CRLF bytes stay unchanged.

Preparation start timestamps were not retained by the frozen measured runner.
Its preparation/preparation-inclusive values are therefore preserved separately
as **worker_reported_timing**, with explicit consistency-only scope. They are not
presented as independently reconstructed clocks, normalized or silently repaired.
This evidence limitation does not change the synchronized/full-target-SHA
completion comparison or establish the separate donor-lifecycle economics claim.

This is exact record/source replay, not hardware attestation or a second rehash
of absent model payloads. CPU-only Job/Pod UIDs, terminal exit/cleanup, pinned Go
image/build, driver SHA and canonical core-source receipts remain separate
required provenance checks before manuscript use. Logical command syscall bytes
are not physical storage writes, device DMA or host-network traffic.

Validation: `python3 -B -m unittest discover -s bench/new-causal -p 'test*.py' -v`.
The evidence tests include matching-smoke, re-mirrored clock/branch/output,
memory-limit and donor-read mutations, plus archive integrity/path checks.
