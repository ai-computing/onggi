# Integrated artifact preparation and native serving plan

> Inline coordination with independent runtime integration and exclusive
> hardware-validation owners; one read-only review of integration changes.

Spec: ../specs/2026-10-30-serving-integration-design.md
Base: Onggi05c97605b07882f9ceaaeb90b94878f8923f81cf.

- [x] Merge exact reviewed69/70/71 heads on a fresh isolated integration branch.
- [ ] Combined Linux race/vet/build, exact source hashes and live S3 CI.
- [ ] Integrate reviewed ATOM/RNGD runtime ancestry, CPU suite and native-ready
      standalone connector/prewarm behavior (runtime owner; RED->GREEN).
- [ ] Freeze both sources/profile/run configuration and read-only preflight.
- [ ] Native smoke, real HTTP request/negative cases and repeat-token proof.
- [ ] UID cleanup/independent absence; sanitized evidence and bilingual docs.
- [ ] Final integrated exact-head CI and publish reviewable branch/PR state.

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
