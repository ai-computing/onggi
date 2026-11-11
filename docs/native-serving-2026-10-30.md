# Onggi preparation to native RNGD HTTP serving

2026-10-30 · [한국어](ko/native-serving-2026-10-30.md)

Evidence: [sanitized native run records](data/native-serving-20261030.json),
[combined CPU/build record](data/serving-integration-cpu-20261030.json).

The integrated path now has a real allocated-card proof: an init container
selects and materializes a compatible graph/config bundle, then a fresh native
prepared worker warms its graph and serves HTTP decode requests from those
delivered files. This validates an explicit-candidate startup path, not a new
controller, live gateway/prefill service or production rollout.

## Frozen inputs

- Onggi `466ef918db08a8722326b6f36f5f39597e8a63de`, combining the reviewed
  resident docs, target-plan Diff optimization and exact-profile preparation.
  Binary SHA256 `6f906b54bdec214957e2b46fa4ff7d737934c7d433f3f465d86bab12d9d972e0`.
- Runtime `fafd820fead20ed51dcb7f60ebb5e7dc7d90b2f1`: shared standalone KV
  transfer, optional `warmup:true`, SDK2026.4 cache and optional two-worker
  host staging. No vLLM test modules or shared SDK package installation.
- Llama-3.1-8B, B4/S256/group2, SDK2026.4.0, Torch2.10.0+cpu,
  fusion8/mesh2, intra-op4, one plugin-assigned RNGD, CPU request4/limit12,
  memory128Gi, deadline7200s. The observed assignment was physical npu1,
  visible `[0,1,3]` -> runtime ordinal1.
- Two previously compiled native graphs total984729 bytes. The bundle also
  carries config/provenance; the16060581181-byte checkpoint stays external and
  read-only. All six checkpoint files and both graphs were freshly rehashed
  against previously accepted hashes. No full-checkpoint delivery is claimed.
- Profiles bind the exact source/options/runtime/SDK/compiler/firmware/layout/
  topology and B/S. The matching manifest is
  `sha256:b36a7d2daf074ca19e66541a4b41b8fa07a4381d0b32f9b28bdb75919cb75f7f`.
  A second candidate with mismatching firmware is rejected with a field reason.
  This is agreement between publisher declarations, not vendor certification.

## Real execution and validity

1. A CPU-only publisher verifies binary execution in the target image, hashes
   original inputs, publishes the bundle/profiles and checks no-match behavior.
2. The serving Pod's actual init runs `onggi prepare`. Every delivered file hash
   equals the published bundle; the main container consumes the delivered
   graph/config volume read-only. Missing/incompatible candidates are not used.
3. A validation-only native producer replays the original four eight-token
   prompts, exports the actual full-width K/V arrays, and creates fresh token
   references. All32 reference tokens also equal prior independent public and
   private fresh-base results. The producer exits before the HTTP worker starts.
4. The genuine cluster worker starts in a fresh process with `warmup:true`.
   Its health endpoint changes503 ->200 after a real graph step. Producer and
   worker use shared-memory BF16 wire data; full native prefix and tail bytes
   are roundtrip-checked before consumption.
5. Three actual batches of four HTTP requests each return eight tokens per
   request. **All12 requests /96 tokens match the fresh references.** Twelve
   first tokens are supplied by the producer; the84 subsequent tokens are
   produced by the native decoder and checked separately.

The consumer makes17 native module calls for startup warmup, then374 total:
`17 + 3 * 7 * 17`. Projection/LM upload remains1 and graph loading1; compilation
is forbidden and stays0. All113 shared projection/LM buffer identities and
compiled module identities are unchanged after each batch. This does not assert
that temporary KV/intermediate buffers have stable addresses.

Checkpoint mismatch returns409 and excess context returns400, with no native
call-count change or KV consumption. The observer only wraps original methods
and checks native output devices/identities; it does not replace computation,
transport, KV or tokens. Timings are instrumented integration diagnostics,
not a controlled cold-start or performance comparison. Cache history and both
attempts must be retained; repeated preparation may use an already warm cache.

The successful harness asserted counter and KV-file-list equality after each
negative request. Individual before/after snapshots for those two negatives
were not persisted; the raw HTTP responses and executed-assertion result are
retained. This distinction is explicit in the evidence.

For reproduction diagnostics, the successful run recorded 13.988 s for combined
native construction/graph loading/warmup and 16.424 s from worker-process start
to HTTP readiness. **Standalone warmup elapsed time was not measured.** Init
preparation took 0.022 s on the retry's existing local store/cache; these values
must not be compared with an uncached full checkpoint load or used as a speedup.

## Retained failed attempt

The first attempt passed init, native reference generation, warmup and negative
admission checks, but its harness sliced K/V down to eight prompt positions.
The actual wire contract requires full S256 arrays, so its four smoke requests
returned400 before native continuation. That run is invalid as a serving proof
and is preserved separately. Only the harness export changed for the second
attempt: full actual native arrays, including untouched tail, with bitwise
roundtrip assertions. Frozen source, profile, checkpoint and budgets stayed
fixed. The corrected96-token run does not hide or average away the first failure.

The runtime's stale prefix-transfer comment was corrected after freezing the
executed source; its parsed Python AST is identical. Reproduce native results
from the frozen runtime commit above. Publication-only docs/comments do not
relabel the tested binary or source revision.

## Evidence and cleanup

The private immutable snapshot contains 205 small evidence files; root verification
rehashed every file and compared all 409 runtime and 125 Onggi tracked-file hashes
with canonical frozen Git blobs. It also independently compared the prior public
fresh-base token arrays, each HTTP token/suffix, graph/buffer identities and the
two canonical artifact manifests. The snapshot manifest SHA256 is
`14a0eebb5f0f8bc850b0705128f43b48ee39564a8ffd7e73e47266356a43f94c`.
The 394278-byte private archive SHA256 is
`70edc475319a3aac8b41f0cc482dacf234b6d474592707993d98fd964800e078`.
The public JSON is a selected, path-sanitized copy, not that archive's bytes.

Checkpoint, graph and 512 KV payload files across both attempts were not downloaded
or published. Their hashes and native export/roundtrip checks come from the owned
remote run; local byte verification applies to the exported small evidence only.
Both final containers exited 0. The HTTP child received SIGTERM after all checks
completed. UID-guarded deletion and an independent API check established that
the namespace and all three owned Pod UIDs were absent. Other workloads were
untouched; no background validation process remains.

## Reproduction boundary and next work

Publish a compatible graph/config bundle whose `prepared.json` points to an
immutable external checkpoint and uses delivered relative cache paths:

```sh
onggi prepare --store "$STORE" --request request.json --cache /cache \
  --dir /prepared/ready candidate-with-wrong-firmware compatible-build
python -m rngd_pd.serving.cluster --role decode --backend rngd --chip -1 \
  --S 256 --sched static --batch-wait 100 \
  --prepared-config /prepared/ready/prepared.json
```

The config opts into `host_workers:2` and `warmup:true`. Follow
[runtime KV replay instructions](https://github.com/ai-computing/rngd-pd/blob/fafd820fead20ed51dcb7f60ebb5e7dc7d90b2f1/docs/prepared-serving.md)
for the actual first token, full wire-width arrays and checkpoint ID; no fake
KV is needed. The validation producer is not required by a normal deployment
with a genuine live prefill producer.

This proof uses one B/S profile and one checkpoint. Multiple native B/S choices,
live prefill/gateway handoff, ATOM HTTP startup, installed-vLLM execution, UCX
recovery, controller discovery/placement/autoscaling and production rollout
remain separate gates. Existing GC lease and crash-lock recovery limitations
remain documented in [compatible preparation](compatible-prepare.md).
The earlier [host-staging speed experiment](https://github.com/ai-computing/rngd-pd/blob/52507e49e57fb9ddf2b8936d76672dc609cdc48c/experiments/2026-10-30-rngd-host-staging.md)
has its own conditions and samples; this integration run claims no speedup.
