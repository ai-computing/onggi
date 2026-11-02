# Stateful A/B/A caller: issue93

Source-only preparation; model cohort not_run. Existing frozen runners, raw,
production/core and all live node/transport sources stay unchanged.

Implement one Linux-only Go main plus real-filesystem tests in this directory.
`runSequence(args, stdout, stderr) int` reads an explicit JSON config and
emits a retained execution record. The common caller uses actual
`csi.Materializer`, `store.Store.Pull` and owner leases. A new root/cache is
created exclusively; an existing or overlapping root is refused.

1. RED tests: actual synthetic A/B store, expected file SHA lists and A/B/A
   requests. P and C must retain initial A/inode and use an actual already-
   published fast return; R must be allowed to consume real owner offers and
   reconstruct the final A from its resulting B/cache state. Assert full
   output bytes, pull/claim counts and no intermediate donor manufacture.
2. Fail closed for unsafe paths, mismatched manifests/expected inventories,
   insufficient retention-allowed reservations and an existing root. Wrong
   expected output SHA must stop the attempted sequence and retain its record.
3. GREEN: implement the common caller. Initial A is an actual deployment,
   separately timed. A real lease is acquired after each verification and
   released only at the next request. A frozen positive idle interval is
   waited normally and charged, never fabricated with Chtimes. P clears the
   offer; C supplies an explicit refusing claim; R delegates the real claim.
4. Each request records relative monotonic and UTC boundaries for release,
   waiting, Ensure, file sync/full SHA and lease, plus actual calls/counters
   and phase occupancy snapshots. The two-switch clock runs from first B
   request to final A verification, including intervening lease/state work.
   Initial deployment plus complete sequence elapsed is reported separately.
5. The first regime admits retaining both complete artifacts under the same
   declared materialized/workspace/cache allowances. No eviction or hard-quota
   implementation is invented. Phase snapshots are not peak/physical I/O.
   Model validity/freezing/resources/n5/order and excluded smoke belong to a
   subsequent outer cohort; this caller does not declare scientific acceptance.
6. Validate Go race tests in the existing Linux CI (local Go unavailable;
   install nothing), read actual expected RED then final GREEN logs and request
   independent source review. Existing Python suite must remain unchanged.
   Publish draft PR only; no remote measurement/card or automatic merge.

The primary evidence unit remains a complete sequence. Failure stops it,
preserves the root and does not produce a replacement successful sample.
Supported deadline budgets are at most600s per request and1800s for both
switches, with context passed into actual Pull and verification. A later
bounded Job supplies an external process deadline as well. No crash recovery
or native readiness is claimed. Constrained capacity/LRU, explicit recovery,
stronger delta baselines and naturally justified native need remain separate
open experiments, not implied by this first source/fixture milestone.
