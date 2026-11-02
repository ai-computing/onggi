# Same-content three-arm materialization (issue88)

Status at source freeze: model experiments **not_run**. This is a new cohort;
do not pool its timings with historical CLI cohorts. No production/runtime code
changes, accelerator allocation, vendor-format parsing or remote launcher here.

The common `three_arm_call.go` invokes unchanged Store.Pull in every arm:

| Arm | Base offer | Claim | Intended operation |
|---|---|---|---|
| P | none | absent | reconstruct from complete target chunk cache |
| C | identical truthful donor | non-null, refuses | copy shared chunks from intact donor, residual from same cache |
| R | identical truthful donor | admits | consume donor and retain same-offset ranges, residual from same cache |

Having no Claim does not test intact-base copying. C must actually report the
expected local_base and positive local_bytes, preserve the donor's full SHA and
inode, and produce an independent target inode. R must report the expected
base and positive reused bytes, remove the donor file and transfer its inode.
All three must have zero CLI pack payload/peer bytes/new cached payload and
match the full frozen target SHA. These are required witnesses, not labels
inferred from flags. Model files are treated as opaque byte sequences.

Primary inputs, prepublished store and complete target cache are exactly the
previous io-frozen configuration (SHA75b2df992e3369f94cb6424c422432dc2155ef9e9af76331fe3eaca5eb563f09).
One ATOM B2 source15,238,278,850B and B8 target15,228,048,882B; hashes remain in
the frozen inventory. Each arm receives independent SHA-checked copies of the
same donor/cache. Verify both immutable inputs before every attempt. Keep
the store immutable and peers disabled. New wrapper loads exact SHA-checked
historical causal.py bytes154680... by direct compile, bypassing cached pyc.

Before the model: actual8MiB/64KiB-revision Go filesystem tests RED then GREEN
under pinned Go1.25.1 (race, vet, gofmt, static driver build); Python treatment,
smoke/source/trace tests; independent source review; actual synthetic CLI push,
complete-target-cache seed, common-driver traced PCR fixture. Synthetic timings
are correctness checks, never model evidence. All outputs use new directories.

Run excluded untraced smoke PCR, then five blocks PCR/RCP/CPR/RPC/PRC. Run a
separate excluded traced smoke and the same five-block traced matrix. Main
requires exact successful matching smoke attempts/order/identities/witnesses
and recomputed summary. Any failed/invalid attempt stops its dependent matrix;
retain failures, never replace them or aggregate only a valid subset.

CPU-only bounded cdx Kubernetes Jobs: etri-001, request4/limit12CPU,
request8/limit32Gi, workers8/GOMAXPROCS32, nice19, no cache drops. Record actual
cgroup/host/affinity/mount/load. Namespace/job/pod UIDs identify ownership;
deadline/TTL/backoff0; release only owned terminal Jobs and verify absence.
No NPU/GPU device request/mount. Remote control only by hub run_remote.

Clocks: command; command through common final target fsync; command through
common full output SHA. Preparation (input validation/donor+cache copying) is
separate and also included in preparation-inclusive completion. Donor SHA/inode
diagnostics follow the common target-ready endpoint and have their own clock.
These are prepared-state measurements; do not claim free donor preparation,
crash durability, device loading or physical I/O. Report median/Q1/Q3/n and all
individual attempts, source/binary identities, environment and raw logs.

Diagnostic logical I/O is successful command syscall return lengths, outside
setup/common later sync/SHA. Store metadata reads remain even with zero pack
payload. Require complete warning-free strace attribution and no artifact
syscall errors. C traced donor reads must cover actual CLI local_bytes; P/R
have no donor-file reads. P/C write full target; R writes a positive strict
subset. Every command must sync target. Keep traced/untraced timing separate.
Preserve and export exact traces only after measurement; independently replay
before using any numbers in a manuscript. No claim of full-set generality,
moved-layout behavior or a final acceptance verdict from this one pair.
