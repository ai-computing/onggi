# Onggi to native prepared serving integration

The user approved continuing the integration/verification sequence. Combine the
reviewed Onggi69/70/71 changes without changing their feature scope, and combine
the reviewed ATOM resident path with RNGD SDK2026.4/host staging. Keep previous
provider sessions/checkouts and all original evidence read-only.

First executable target: an allocated RNGD Pod whose init prepares an explicitly
compatible graph/config bundle and whose real HTTP prepared worker consumes
only those delivered graphs/config. Checkpoint files remain mounted read-only
from the previously verified base; this is not full-checkpoint delivery.
No new compile, SDK/driver install or vLLM test stub is acceptable. Reuse the
real standalone whole-request transport with optional native prewarming before
serving readiness. Existing vLLM plugin users retain their contract.

Alternatives considered: deploy all controllers immediately (unimplemented
contracts), download/rebuild a whole checkpoint (does not isolate this graph
delivery question), or a bounded init-to-native-worker run. Choose the last;
the implementer owns routine configuration choices under continued execution.

## Run contract

- Freeze source commits and verify binary/source hashes. Use hub.orca.run_remote
  only for remote work, through established rebellion/dell_0 routes.
- Unique owned run directory/namespace; one plugin-assigned RNGD, CPU request4/
  limit12, memory128Gi, active deadline7200s. Hardware owner alone allocates,
  launches and cleans with UID preconditions. Do not touch foreign workloads.
- SDK2026.4/Torch2.10, group2/B4/S256, intra-op4, existing compatible immutable
  graphs and checkpoint. No group8 or old-SDK causal comparison.
- Bundle contains two cached native graphs, configuration and provenance.
  Profile records exact model/options/runtime/SDK/compiler/firmware/topology.
  Include a mismatched candidate to prove actual selection, not only raw pull.
- Native producer replays real prompts, exports true layer KV/first tokens and
  independent fresh continuation, then exits before a fresh HTTP worker starts.
  No synthetic zero KV may be described as prompt prefill.
- Worker is genuinely standalone when vLLM is absent; shared SDK remains intact.
  Opt-in warmup executes every configured variant and discards warmup KV before
  readiness. Configuration defaults remain unchanged.
- Smoke batch4x8 tokens, then2 repeat batches if valid (12 requests/96 tokens
  total). Compare every token, including all native-produced tokens after the
  supplied first token. Check readiness, immutable checkpoint ID admission and
  no native compile/new weight upload on repeated prepared requests.
- Measure file preparation, process/native preparation, warmup and request
  intervals separately. All attempts/raw failures retained. No speedup or
  statistical claim from this integration run.
- Cleanup all owned Pod/namespace/processes and independently verify absence.
  Retain private run logs and publish sanitized hashes/results only, no vendor
  binary/IR, machine paths, secrets or prior provider session changes.

After RNGD proof, publish the integrated branches, exact-head CPU/S3 CI and
document the remaining ATOM/native-controller/GC-crash-recovery limits. Main
integration must be based on concrete green combined checks; no blind merging
of unresolved upstream draft branches or shared deployment changes.
