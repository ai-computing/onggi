# Compatible artifact preparation implementation plan

> Execute inline using executing-plans; one fresh whole-branch reviewer after
> implementation. Continued execution and feature-branch push/PR are authorized.

Goal: select explicitly compatible artifacts and prepare verified files before
serving startup. Go stdlib plus existing dependencies only.
Spec: ../specs/2026-10-30-compatible-prepare-design.md
Base: origin/main 05c97605b07882f9ceaaeb90b94878f8923f81cf.

## Constraints and review focus

No SDK/version guessing, shape padding or NPU-ready claims. Legacy unprofiled
push/pull unchanged. Optional manifest profile is hash-bound, unknown fields and
incomplete identities refused. Requests/candidates validated before file writes.
GC lease spans ref resolution through atomic publication. Readiness only after
all hash checks; destination/cache failures cannot silently return ready.
Review: malicious/incomplete metadata; ref TOCTOU; partial publication/cancel;
concurrent destination ownership; compatibility field omissions and legacy data.

## Task 1: Profile publication and selection

Files: pkg/store/{profile.go,profile_test.go,format.go,push.go};
cmd/onggi/{store.go,prepare_test.go}.
Interfaces: Profile.Validate/DecodeProfile, Store.Select(ctx, []digest.Digest,
Profile) -> Selection/error; Manifest.Profile, PushOptions.Profile; push --profile.

- [x] Verify clean Linux baseline on an owned CPU work directory.
- [x] RED: exact identity matching, legacy exclusion, invalid profile/JSON,
      deterministic candidate choice, bad-manifest fail closed, profile roundtrip.
- [x] Implement bounded metadata validation, optional profile and selection.
- [x] GREEN focused tests, full suite, commit with validation ledger.

## Task 2: Atomic CLI preparation and operator documentation

Files: cmd/onggi/{prepare.go,prepare_test.go,main.go}; README.md/README.ko.md;
docs/{compatible-prepare.md,ko/compatible-prepare.md}; deploy/k8s example.
Interface: prepare CLI per spec, always JSON report; request shares Profile v1.

- [x] RED: candidate selection/immutable digest, no-match/no cache writes,
      plan-only, hash corruption/cleanup, existing/busy/symlink destinations,
      strict profile errors, byte-identical pull and warm node cache reuse.
- [x] Implement prepare via existing Pull/Cache/Peers, sibling temp+mkdir lock,
      store GC lease and atomic rename; never use shared CSI Materializer.Init.
- [x] GREEN focused tests, full race/vet/build; bilingual docs and examples.
- [x] One independent branch review, fix important findings with RED->GREEN.
- [x] Commit/push draft PR #71; final publication-head CI gate recorded in PR.

## Ledger

- New isolated worktree; prior provider/checkouts untouched. Ruling: first P3
  increment is explicit-candidate exact-profile selection + disk preparation;
  full discovery/controller/compile farm and native device warmup remain later.
  This avoids inventing cross-repo contracts or guessing vendor compatibility.
- Ruling: strict exact B/S means callers must publish each supported profile;
  padding/bucketing is only safe when the runtime explicitly supports it.
- Baseline Linux `go test ./...` passed on clean 69033a1. Owned CPU-only
  work directory and existing private Go1.27.3 toolchain, GOMAXPROCS4; no NPU,
  shared SDK or driver operations. New GOPATH/GOCACHE isolate this work.
- Task1 RED at56f6865: missing Profile/Select/PushOptions fields. GREEN at
  c46041a: every exact identity field, legacy ordinary pull, missing manifest,
  deterministic/deduplicated matches, strict incomplete/unknown JSON and
  push roundtrip pass. Profile validation precedes push data/lease operations.
- Task2 RED ateb1862f: missing prepare/prepareFiles. GREEN at1d050e5
  (gofmt only afterwards): real CLI publication/restoration byte equality,
  second pull cache reads with zero store bytes, plan/no-match no target/cache,
  existing/file/symlink/busy refusal, corruption cleanup and selected digest
  preserved after actual ref CAS change. Profile errors precede backend open.
- Full Linux `go test -race -json ./...`:148 test/subtest pass events, no failed
  tests/packages. `go vet ./...` and Linux CLI build passed. Live S3 conformance
  is skipped locally without servers; final CI runs both actual S3 backends
  plus the new profile/prepare/cache CLI workflow before publication completes.
- Ruling: preserve artifact file permissions, refuse existing destinations
  rather than silently trusting them; process-crash temp/lock recovery remains
  explicit and documented. This costs manual recovery/new destination on a
  crash, and avoids treating an unverified existing directory as ready.
- Final review at1f1ca21 found three Important issues, no Critical/Minor.
  All reproduced together RED atee6e457: partial-suffix files overwritten in
  reverse-order manifests; temporary tree mode0755; locator count4294967295
  returned despite error. Fixed c7e3910: private tree0700 until verification,
  unique0600 staging filenames excluding all final/ancestor paths, duplicate
  final-path refusal, and locator count0 on error before allocation. GREEN
  focused regressions and full race/vet/build;151 test/subtest pass events,
 10 environment-dependent skips, no failed test/package. Maximal allocation
  was never executed. Byte hashes for72 Go/source-dependency files independently
  match local tested files. Evidence: docs/data/compatible-prepare-20261030.json.
- Final: Ruling on reviewer-declined publisher truth/omitted vendor dimensions,
  ABI, native readiness and latency: metadata exact agreement only; publisher
  records complete build identities/options and runtime validates artifacts.
  No new native claim; the cost of inaccurate declarations is load rejection
  or incorrect vendor compatibility, not something this selector can certify.
- Final: Ruling on hostile parent replacement/noncooperating writers: lock
  coordinates cooperating preparers only; deployment uses owned directories.
  Adversarial filesystem isolation requires a separate hardened filesystem API.
- Final: Ruling on crash durability/stale-lock recovery/existing adoption:
  retain explicit documented operator recovery and refuse existing targets;
  no crash-durable or automatic recovery claim. Cost: manual retry preparation.
- Final: Ruling on strong concurrent GC exclusion: reuse existing TTL/snapshot
  lease protocol, not a reader/writer lock. Document24h default TTL and retain
  immutable refs/coordinate critical GC; a future protocol change is separate.
- Final: Ruling on S3/Kubernetes/hardware review limits: both real S3 servers
  passed CI41003744490 before fixes; final exact-head CI must pass again and
  is recorded in the PR. Kubernetes example is syntax-checked, not deployed;
  no NPU hardware operation, native warming or new performance claim.
- Owned CPU suite runner has exited; no NPU allocated. Keep isolated source/
  logs/cache for inspection. Final publication keeps the branch/worktree and
  records CI in PR metadata rather than creating another CI-only commit.
