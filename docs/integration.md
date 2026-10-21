# Integration with ai-rm, lmd-top and llm-d

Status: draft v0.1 (2026-10-21) · [한국어](ko/integration.md)

Onggi joins the [ai-rm](https://github.com/ai-computing/ai-rm) stack through [rm-core](https://github.com/ai-computing/rm-core)
contracts, like every other component: no component imports another. Engine internals (Optimus Prime, FASOP in
[aicomp](https://github.com/ai-computing/aicomp)) are changed only by their owners; onggi asks through issues.

## Per repository

| Layer | Repository | Onggi provides | Onggi uses | Work |
|---|---|---|---|---|
| contract | rm-core | — | `ArtifactStore` contract, manifest schema, `airm_artifact_*` metric names | rm-core PROTOCOL v0.2 section + conformance |
| L1 | rm-accel | per-node holdings (`NodeArtifactCache`) | device, SDK, compiler and firmware versions → compatible artifact key | AcceleratorProfile fields |
| L2 | hetero-npu-p2p | — | — | later: DMA-BUF for host → NPU weight loads |
| L2 | npu-offload | per-expert fetch of MoE weights (cold experts from the store) | expert hit statistics → hot/cold hints | expert-granular manifests; store-backed loader |
| L3 | rngd-pd | mount of compiled graphs and weights per profile | required profiles (S, B, TP) | loader reads an onggi volume instead of fixed paths (first consumer) |
| L3 | npu-train | base weights, compile cache | trained adapters and checkpoints → new `ModelSource` | publish on training end |
| L3 | Optimus Prime · FASOP | one artifact interface for the engine | FASOP cost model → which profiles to compile | **feature requests in aicomp only** |
| L4 | rm-controller | artifact readiness in the placement cost | placement decisions (`airm-placement`) → prefetch | readiness term; prefetch hook |
| L4 | hetero-llm-d | replaces the shared model store | scale-up signals → pre-warm | manifests use onggi volumes |
| L5 | rm-bench | — | `artifact-coldstart` scenario | new scenario |
| L5 | lmd-top | data and actions for the Deploy view | — | see below |
| hub | ai-rm | — | pinned version, umbrella chart, layer map | submodule, chart, docs |
| analysis | trace analysis | — | request-length distribution → which profiles to keep | input to semantic compaction |

## lmd-top

lmd-top's Deploy view already does a first version of this: Library (store builds, the toolchain recorded in each
artifact, compile/prefetch/deploy, move/remove/rescan as reviewable Jobs), Zoo (vendor model lists), a compile
advisor (`npu-compat.json`, invalid-option prediction, compile history) and a discovery CronJob that publishes the
`model-inventory` ConfigMap every 10 minutes. With onggi, **onggi is the source of truth and executor; lmd-top is
the operator console**:

| Today in lmd-top | With onggi | lmd-top then |
|---|---|---|
| discovery CronJob → `model-inventory` ConfigMap | holdings live in CRDs | Library reads CRDs (no 10-minute lag); onggi keeps publishing the ConfigMap during migration |
| compile Job generated directly | `CompileRequest` / `ArtifactSet` → compile farm → chunked publish | pre-flight and progress; results deduplicated |
| move/remove with `mv`/`rm -rf` Jobs | ref operations + GC (immutable data, in-use artifacts protected) | safer actions; reclaimable space preview |
| prefetch into the shared store | per-node prefetch, peer-to-peer, missing chunks only | per-node prefetch progress |
| provenance via a probe Job | toolchain, options and source digest in the manifest | read from the manifest |
| `npu-compat.json`, option prediction, history | moved into onggi's compile planner | advisor UI stays, data from onggi |

New views: logical vs stored size per build and what it shares; a node cache map (hot/warm per node); a cold-start
timeline (materialize → load → ready); compaction proposals with reclaimed space and padding cost.

## llm-d

| llm-d piece | Attachment | Effect |
|---|---|---|
| deployment (ModelService charts, hetero-llm-d manifests) | pod templates mount `onggi` volumes | one delivery path for every engine |
| llm-d-autoscaling, KEDA | scale-up signal → pre-warm target nodes; onggi reports per-node readiness cost | lower scale-from-zero cold start |
| llm-d-router / EPP (InferencePool) | LoRA adapters served for dynamic loading next to adapter affinity; readiness for scale-out | adapters and scale-out, not per-request routing |
| llm-d-kv-cache | later: prefix KV blocks in the content-addressed chunk layer | persistent prefix store (study) |
| llm-d-benchmark, inference-sim | cold-start benchmark; controller tests without NPUs | verifiable in CI |
| llm-d-go-template | onggi's Go services follow it | upstream-friendly from the start |

Rule: only extension points (CRDs, CSI, webhooks, InferencePool/EPP plugins, metrics). Neither Kubernetes nor llm-d is
forked or patched.

## Order

| Phase | Onggi | lmd-top | llm-d / ai-rm |
|---|---|---|---|
| P0 | dedup-report; import of the existing store layout | capacity and dedup view | — |
| P1 | chunk store + CLI | Library reads CRDs alongside the ConfigMap | rm-core `ArtifactStore` draft |
| P2 | agent + CSI volume | node cache map, cold-start timeline | rngd-pd loader on an onggi volume |
| P3 | CRDs, compile farm, compile planner | compile and prefetch create CRs | rm-accel compatible key; npu-train publishes |
| P4 | prefetch and placement | — | rm-controller prefetch; autoscaling pre-warm; adapter serving; rm-bench scenario |
| P5 | semantic compaction; expert-granular fetch | compaction proposals | npu-offload expert fetch; kv-cache study; aicomp requests |
