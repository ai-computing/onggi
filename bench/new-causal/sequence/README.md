# Stateful CPU file-owner execution

This is a separate benchmark caller for issue93. Production code and the
existing one-step runners/evidence are unchanged. Model cohorts are **not_run**.

Run the real tiny-file contracts in an existing Linux Go environment:

```sh
go test -race -count=1 -v ./bench/new-causal/sequence
go build -mod=readonly -trimpath -o /new/scratch/file-sequence ./bench/new-causal/sequence
/new/scratch/file-sequence --config /new/scratch/config.json
```

Tests publish an actual deterministic8MiB A/B pair to a real store and exercise
initial A, requested B, requested A. P omits offered bases. C passes bases with
an explicit refusing claim, permitting intact-base copying. R calls the real
Materializer claim, retaining its lease/age recheck and private transfer.
The expected A/B manifests and whole-file SHA values are independent input.

The owner root and cache begin new. Initial A is an actual deployment through
the same materializer, not a copied donor. After verification it receives a
real owner lease. The next request releases that lease. Only a missing target
for C/R waits the frozen positive idle interval; P and an already retained
target need not wait for a donor they do not use. No Chtimes/age fabrication,
intermediate reset or replacement donor is used. No volume is mounted and no
service request/native runtime is simulated by the lease record.

P/C can return the existing A with zero additional Pull calls; full target
sync/SHA remains common. R must return using the files/cache that survived B,
which can require a new Pull. Tests check both paths and complete output bytes.

## Config and admission

`status=not_run`, `kind=fixture|model`, `policy=P|C|R`, `store`, new `root`,
two `artifacts` keyed by labels, and `requests=[A,B,A]` are explicit. Each
artifact contains its immutable `manifest` and complete `files` records with
`path`, `bytes`, lowercase SHA256. Workers, `idle_ns`, `step_timeout_ns`,
`sequence_timeout_ns`, `materialized_capacity_bytes`,
`workspace_capacity_bytes` and `cache_capacity_bytes` are mandatory.

The origin must be an existing, immutable store with regular config and
existing tmp/refs directories. Layout admission precedes the backend open,
so mistaking an ordinary input directory for a store does not create entries
inside it. Config contents are then checked by the actual store reader.

The primary regime must allow retention of both complete artifacts. Workspace
reserves at least the largest target plus one configured cache pack. These
are common declared allowances, **not kernel-enforced quotas**. Occupancy is
observed at quiescent phase boundaries, not continuously. Exceeding a phase
allowance fails the attempt; passing snapshots does not establish a physical
peak or constrained-capacity behavior. No eviction/LRU policy is implemented.

Model invocation additionally requires `binary_sha256` in the config and an
external `--config-sha HASH` matching exact config bytes. Source/build/input/
store/cache/resource freezing, initial-state admission, excluded smoke, the
five complete-sequence attempts and fixed order remain the outer experiment's
responsibility. These pins alone are not scientific acceptance or hardware
attestation. The caller never produces a model-cohort aggregate or valid-subset
result.

## Boundaries and retained state

`root/result.json` is updated with exact config identity, actual store config,
request UTC and monotonic offsets, release/wait/Ensure/sync/full-SHA/lease
boundaries, real Pull/claim counters, file device/inode and phase occupancy.
`sequence_elapsed_ns` is one interval from B request to final A hash completion.
`preparation_inclusive_ns` starts at caller entry, includes initial setup/A and
ends at that same final A hash; it does not include prepublication, process/Pod
startup or a native first token. Final A lease/snapshot follow the target hash
endpoint and are separately recorded. Earlier intervening lease/state work
falls inside the whole-sequence interval.

Whole-file SHA and synchronization occur for retained fast returns too. CLI
`stats.bytes` is logical target size, not measured writes. Store/cache counters
retain their stored-form semantics. Phase logical file occupancy deduplicates
hard-link aliases within a category; cache is accounted separately. Physical
disk/network/DMA, RSS/peak workspace and byte-seconds are not inferred here.

At most600s per request and1800s for both switches are admitted. Deadlines reach
actual Pull and hash checking; Ensure waits on its actual owner completion,
rather than returning a timeout while background construction still runs.
A future node Job also needs an external process deadline. Failure preserves
the root and observed attempts, stops the cycle, and does not attempt automatic
recovery, donor regeneration or a replacement success. Failed/unreached time
fields must not be plotted as zero-duration successful observations.

Source/fixtures establish behavior only. Operational usefulness after native
sharing, stronger direct delta baselines, other directed layouts, constrained
budgets, explicit recovery and model n5 measurements remain separate open work.
