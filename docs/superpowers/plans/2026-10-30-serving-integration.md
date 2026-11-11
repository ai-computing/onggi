# Integrated artifact preparation and native serving plan

> Inline coordination with independent runtime integration and exclusive
> hardware-validation owners; one read-only review of integration changes.

Spec: ../specs/2026-10-30-serving-integration-design.md
Base: Onggi05c97605b07882f9ceaaeb90b94878f8923f81cf.

- [x] Merge exact reviewed69/70/71 heads on a fresh isolated integration branch.
- [x] Combined Linux race/vet/build, exact source hashes and live S3 CI.
- [x] Integrate reviewed ATOM/RNGD runtime ancestry, CPU suite and native-ready
      standalone connector/prewarm behavior (runtime owner; RED->GREEN).
- [x] Freeze both sources/profile/run configuration and read-only preflight.
- [x] Native smoke, real HTTP request/negative cases and repeat-token proof.
- [x] UID cleanup/independent absence; sanitized evidence and bilingual docs.
- Final publication gate: push reviewable branch/PR state and require exact-head
  CI before completion; record final CI URLs/results in both PR bodies.

## Ledger

- Onggi heads69=fcf2fb8,70=358c755,71=db82b4c merged with no conflicts;
  combined source dafcfe16e8aaaed069f7471f2b6896f0b47a4176. Initial fetch did
  not populate remote refs in the single-branch clone; merged verified local
  commit IDs instead, preserving all original heads and worktrees.
- Ruling: graph/config delivery only, original checkpoint external read-only;
  a full16GB checkpoint transfer would answer a different question and is not
  claimed. Run records separate file readiness, native readiness and service.
- Read-only preflight found no vLLM in the shared SDK environment. Ruling:
  factor genuine standalone whole-request transfer/prewarm support with real
  tests; do not inject test stubs or install packages into the shared SDK.
  rm_core already has a genuine builtin transport fallback.
- Combined Onggi466ef918: clean Go1.27.3 trimpath binarySHA256
  6f906b54bdec214957e2b46fa4ff7d737934c7d433f3f465d86bab12d9d972e0,
 27245207 bytes;154 test/subtest pass events,10 environment-dependent skips;
  format/race/vet/build passed. CI41005576721 all3 Go/S3 jobs passed. Root
  independently compared executed source hashes with canonical Git bytes.
- Integration review: all69/70/71 ancestry preserved; production exactly71
  except reviewed Diff implementation70. NoCritical/Important integration
  finding. Documentation wording reconciles implemented explicit-candidate
  CLI selection versus planned discovery/controllers/native orchestration.
- Runtime finalfafd820fead20ed51dcb7f60ebb5e7dc7d90b2f1 merges reviewed
  ATOM28 into RNGD32; genuine standalone shared connector and optional startup
  warmup added. Fresh review's cancellation/cleanup issue regraded Important:
  preserve original exception+cleanupcause, mark RNGD failed before fence,
  retain buffers if fence fails, retry close after recovery.3 RED->GREEN;
  full local390passed8skip4subtests, final LinuxCI41006516072
 396passed2skip4subtests18.91s. Root15warmup regressions also passed.
- Frozen model-source profile digest2b6e92c792503258f3d9f13633a0d94a69387b31964cd54f17f857d52965c6c8
  derives canonical6file BASE-only manifest, independently matched old public
  and private accepted records. Graphs total984729B; external checkpoint
 16060581181B. Exact SDK/compiler/firmware/options remain publisher declarations;
  the native proof establishes only this observed compatibility.
- Native replay contract separates12 provided first tokens from84 produced
  suffix tokens (96 total) over12 HTTP requests; independent old fresh-base
  reference comparison required in addition to current native producer.
- Review-declined native allocation/KV/HTTP/cleanup gates remain mandatory;
  old unchanged code was not recertified beyond this configuration. Installed
  vLLM runtime, UCX recovery, arbitrary shapes, performance/SLO and production
  rollout are not established. Existing GC/crash-lock limitations remain.
- Completed native gate: frozen466ef918/fafd820, real init selected matching
  bundle and rejected firmware mismatch; all delivered file hashes match.
  First prefix-only KV harness failed four requests before native continuation;
  preserved separately. Corrected full native S256 KV roundtrip, same frozen
  conditions, new owned Pod:12HTTP/96tokens match fresh producer and independent
  prior public references,12provided+84native suffix tokens. Native17 before
  readiness then136/255/374;113shared/LM buffers and compiled IDs unchanged,
  upload1/graphload1/compile0. Negative409/400 executed counter/KV assertions;
  no individual negative snapshots persisted. Standalone warmup latency unmeasured.
- Completed cleanup/evidence gate: all owned UIDs/namespace absent by independent
  API; no background native worker. Root rehashed205 exported small files,
  canonical Git409runtime/125Onggi file hashes, HTTP/reference arrays, profiles,
  manifests and buffer identities. Large native payloads stay remote-only.
  Public docs/data/native-serving-20261030.json retains both attempts and limits.
- Publication is documentation plus one runtime comment correction; all runtime
  src ASTs match frozenfafd820. Exact final-head CI links belong in the PR bodies,
  avoiding a CI-result-only commit loop. No main merge or production rollout.
