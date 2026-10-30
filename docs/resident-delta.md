# Onggi to resident NPU updates

2026-10-30 · [한국어](ko/resident-delta.md)

Onggi finds and delivers changed file content. The serving runtime decides how
those changes map to device tensors and performs the resident update. The
RNGD row-update implementation is in
[rngd-pd PR #29](https://github.com/ai-computing/rngd-pd/pull/29), not in Onggi's
Go storage engine. That PR remains unmerged at the time of this record.

## Where Onggi stands

Onggi is at P2: the content-addressed store and node delivery/CSI path are
implemented. POSIX/S3 push/pull, hash verification, changed-range diff, local
base reuse, node cache/peer transfer, lease-protected GC and read-only CSI
materialization have code and validation. The current main also bounds
concurrent materialization and preserves lease ownership across failures.

P3's Kubernetes artifact/ref APIs, compile farm and compatibility planner are
still planned. P4's placement/autoscaling-driven prefetch and prewarm are also
planned. Reconciliation of leases conservatively left by crashed processes
remains a limitation. See the [integration phases](integration.md#order) and
[CSI safety evidence](validation-2026-10-23.md).

Prepared B/S graph selection and native resident weight updates belong to the
serving runtime; they are not automated by Onggi's storage layer.

## The implemented path

1. Publish matching immutable checkpoint directories as Onggi refs.
2. `onggi diff --ranges --json` identifies changed file ranges.
3. rngd-pd maps/refines ranges into safetensors rows and validates the checkpoint.
4. Its `SwappableDecode` stages changed rows and matching rollback data, then
   copies into existing RNGD allocations while retaining the decode graph.
5. Start new requests with fresh KV after the drained update boundary.

For projections, source rows become columns in the transposed resident tensor.
LM-head rows remain contiguous and split at packed chunk boundaries. Dense or
fragmented plans conservatively update the whole tensor. A few changed values
therefore mean selected complete rows, not arbitrary byte writes to NPU memory.
Failed updates restore the old state; failed rollback disables serving.

```bash
onggi push --store "$STORE" --ref base "$BASE_CHECKPOINT"
onggi push --store "$STORE" --ref new "$NEW_CHECKPOINT"
onggi diff --store "$STORE" --ranges --json base new > ranges.json
```

The runtime can invoke Onggi directly:

```python
result = engine.load_delta(
    new_checkpoint_dir, store=store, base_ref="base", new_ref="new", onggi="onggi",
)
```

The refs must describe those exact checkpoint directories, which remain
immutable throughout planning and application. Pulling files, initial store
publication, prepared-shape selection and live device mutation have separate
costs. CSI materialization alone does not perform this device update.

## Verified RNGD measurements

Measured 2026-10-23 on one RNGD, Llama-3.1-8B-Instruct revision
`0e9e39f249a16976918f6564b8830bc894c89659`, B4/S256, fusion 8/mesh 2,
furiosa-torch 2026.2.0 and Torch 2.11.0+cpu. Baseline code:
`da85c80ba5f0eb979d32bbcbbbcd4b1de13b0c13`; optimized code:
`719c09b0f105782ffbf75ed3451ec9337d7545f2`. Onggi main:
`05c97605b07882f9ceaaeb90b94878f8923f81cf`.

Both implementations ran `new → base → new → base` on each of three fixtures.
All **24 updates / 768 generated tokens** equaled independently fresh-loaded
references. Each destination changed the base output. Optimized runs retained
the original shared/LM allocations and compiled modules without graph preparation.

| Fixture | Logical upload, old → new | Complete update median, old → new | Through-first-token median, old → new |
| --- | ---: | ---: | ---: |
| Complete down projection | 117,440,512 → 117,440,512 B | 0.687 → 1.149 s | 0.918 → 1.475 s |
| Down projection, 64 rows | 117,440,512 → 1,835,008 B (64× smaller) | 0.874 → 0.523 s | 1.232 → 0.832 s |
| LM head, 8 rows | 1,050,673,152 → 65,536 B (16,032× smaller) | 1.018 → 0.299 s | 1.315 → 0.587 s |

Complete update includes live Onggi comparison, planning, checkpoint handling
and application; through-first-token adds the first-token interval for an
eight-token prompt. Medians include all four updates, including the first.
Payloads are logical BF16 bytes, not PCIe/DMA measurements. Host load varied:
the complete-tensor candidate was slower, so no full-update or decode speedup
is established. Sparse application itself took 7.1–10.3 ms for projection rows
and 1.20–1.77 ms for LM rows; Onggi/checkpoint work remains outside those intervals.

[Summary with every update sample](data/resident-delta-20261023.json).
[Frozen protocol, full tokens, source hashes and cleanup evidence](https://github.com/ai-computing/rngd-pd/blob/75252049416ee65397113afbcaddc11d71a7f6d2/experiments/2026-10-23-rngd-row-update.md).
The companion [ATOM work](https://github.com/ai-computing/rngd-pd/pull/28) uses a
different device-input runtime path; these RNGD numbers do not describe ATOM.

## Furiosa SDK 2026.4 assessment

SDK 2026.4 is a useful migration target: official release notes describe reduced
DMA/stream overhead. Furiosa-LLM requires the 2026.4 driver and loads models
through FXB rather than its removed legacy artifact path. These published
changes do not establish faster resident updates in this custom engine.
[Release notes](https://developer.furiosa.ai/latest/en/whatsnew/release-2026.4.0.html),
[upgrade guide](https://developer.furiosa.ai/latest/en/get_started/upgrade_guide.html),
[FXB guide](https://developer.furiosa.ai/latest/en/furiosa_llm/fxb.html).

A read-only preflight on 2026-10-30 found an existing isolated SDK 2026.4.0
environment with Torch 2.10.0+cpu and installed 2026.4 driver/firmware packages.
Torch/Furiosa and rngd-pd's version adapter import successfully. However,
`furiosa.native_torch`, `ir.Edf` and `EdfModule` are unavailable, while the current
`artifact_cache.load` requires the old native EDF loader. Import success is not
successful model loading. The loaded kernel-driver version was not independently
resolved by this check; installed package versions alone do not prove it.
[Preflight record](data/sdk264-preflight-20261030.json).

The subsequent [loader migration PR #31](https://github.com/ai-computing/rngd-pd/pull/31)
adds atomic SDK/Torch-checked native Runnable caches to rngd-pd. Fresh-process
native tests preserve duplicate, passthrough and empty outputs, including a
nonzero-offset/noncontiguous passthrough. That custom cache is separate from
Furiosa-LLM's FXB loader; full-model configuration limits and validation are
tracked in that PR. The earlier preflight record above describes the baseline,
not the later migrated loader.

Migration also requires separately versioned artifacts, stream-completion/
rollback checks, and fresh-token parity plus timing on an allocated card. Do not relabel
the 2026.2 results as 2026.4 results or reuse the old EDF cache by renaming its
directory. The SDK preflight did not allocate an NPU, run inference, install
packages, replace the working SDK, or change the shared host.

Onggi stores FXB files as opaque content like other artifacts; it does not need
to decode their vendor format. Runtime compatibility selection and live FXB
weight replacement are separate integration work, not automatic capabilities
of file deduplication.
