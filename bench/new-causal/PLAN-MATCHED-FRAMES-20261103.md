# Matched full-set publication implementation plan (issue90)

**Goal:** test flat fine CDC against hierarchical CDC under identical zstd,256KiB frame,64MiB pack and CPU conditions on the exact19-file/263893021566-byte ATOM family. No two-file result substitutes for full-set isolation.

**Architecture:** a new stdlib runner directly compiles the hash-pinned historical helper, without editing its source or earlier results. Each policy publishes the same single family directory to a fresh store, records finalized payload/metadata and actual config, pulls to a separate directory and checks the complete inventory. Successful artifacts are cleaned only inside the owned trial after their raw witnesses are saved. Failures remain. A synthetic multi-file CLI fixture tests the real path without claiming model performance. After the original unchanged-core approach failed actual CI, a separately generated experimental reference admits an identity-split-only exception in one guard; product sources remain unmodified. Exact source delta and build receipt are mandatory for model use.

**Tech stack:** Python3.10+, existing Linux Onggi CLI/GNUtime/nice; no installation/device/core changes. Parent hub owns remote execution. Existing linked worktree is reused on exp/90-matched-frame-policy from immutable1e3c74f; issue90 preceded branch creation. Original exp88 remains pinned, and its exporter already froze exact local verifier copies before this branch switch.

**Spec:** private expanded agenda matched-encoding/full-set gate and original simulated-review Z2/B3/L4/V4. This public plan contains no manuscript or reference-PDF payload.

## Tasks

- [x] Baseline73Python tests69pass4platformskips; actual prior Linux CLI/Go checks remain under PR89, not rerun evidence for new code.
- [x] TDD policy/config/order/smoke/source/output-overlap/full-set gates; add small actual CLI fixture test (explicit ONGGI_TEST_BIN, otherwise skip).
- [x] Implement new runner and exact raw witness output. Four fixed policies D/I/F/H in rotated blocks; excluded smoke1 then main5, failed/invalid stops without replacement. D is direct fine/frame0, I is identity-sub fine/frame0, F is identity-sub fine/frame256KiB, H is hierarchy/frame256KiB. The identity adapter must be proven to preserve every fine chunk list and produce no composites; D/I exposes its overhead.
- [ ] Verify full actual store chunker/subchunker/frame policy, command/env, complete push file/byte counters, finalized-object inventory and full output equality. Retain source hashes/config/tool/environment/timestamps and GNUtime. Preparation, push and roundtrip clocks remain separate.
- [ ] Independent whole-branch review, local tests, draft PR and all CI logs; source fixture success is not scientific acceptance.
- [ ] Only after existing measurement/export release: live input/free-space preflight, bind exact19-file inventory, freeze, excluded model smoke, rotated main20 CPU-only bounded Jobs. Exact export/replay and separate provenance review before paper metrics. No new model run is authorized to overlap current traces or transport.

## Constraints and decisions

Ruling: the original full-set storage experiment pushes19files in one artifact, so this runner uses one exact family directory, rather than silently changing the study to a multi-reference sequence. Model freeze requires19files and total263893021566B plus a caller-predeclared complete hash inventory; actual paths/hashes must still be verified before launch.

Ruling: successful full-set roundtrip trees and stores are removed after raw recording to bound disk use, while failed trees and records remain for diagnosis. Before push require a conservative full-input store budget plus margin. After actual store size is known, require enough remaining space for the complete roundtrip plus margin before pull; otherwise fail without pretending validation succeeded. Never rely on the earlier favorable compressed ratio.

Review focus: flat policy accidentally retains subchunking; stored frame differs; CLI silently publishes a subset; smoke lacks real trial witnesses; source pyc substitution; changed immutable input; treating build/fixture green as model validity; cleanup escaping owned output; missing CPU/RSS measurements represented as zero; free-space bound insufficient for incompressible input.

Ruling: --sub0 disables frames even when --frame is present. Use an identity sub-split (min64KiB exceeds fine max16KiB) to enable the existing frame builder without changing flat chunk identities. D/I no-frame controls expose extra identity-adapter work; publication timing cannot be advertised as an optimized native flat-framing baseline. The same selected file kinds and frame acceptance logic apply to F/H. Actual index frame/composite records and fine per-file list equality must be verified by the real fixture before model launch.

Ruling after Important review: product OpenWith rejects the identity sub-split. InitialCI41486253528/mergead988ff actually failed its I fixture (80tests, onefailure); the complete decoded Go log is retained by the parent. Do not weaken the store's existing validation in this checkout. Instead generate pinned cmd/pkg source in an external fresh tree and narrow the copied guard to `Sub.Max > p.Min && Sub.Min < p.Max`. This admits the identity case only; the existing too-large non-identity test remains valid. Label the result experimental reference, preserve all source/base/delta/Go-test/binary identities and do not pool it with historical production measurements.

Ruling after Important review: marker/summary-only smoke admitted incomplete rows. A reproduced RED now requires actual stored policy/index, complete CLI/manifest/input/output, successful push/pull argv/environment and actual D/I/F list equality in check_smoke(cfg). These checks are separate from later exact raw export/replay and build/control provenance acceptance.

Ruling after Important review: a4-field status-only build receipt admitted an unexecuted binary in a synthetic freeze test. After reproduced RED, reference validation binds base/generator/all67file identities/exactguard/commands and all8raw logs. The checked generator is directly compiled; receipt and logs accompany cohorts. Local85tests79pass6platform/CLI skips; actual reference execution and independent review remain separate pending gates.
