# Which files get sub-chunks (#74)

Two-level chunking (#15) re-splits the chunks a push finds new into ~4 KiB sub-chunks, so files that hold the same
bytes in a different order still dedup. Until #74 a new store chose those files **by name**: `--sub-kinds
atom-prefill,atom-decode,atom-other`, which `pkg/layout` derives from the `.rbln` extension and the words `prefill`
and `decode`. Every other format, such as TensorRT-LLM engines or furiosa-llm bundles, got one level only, and a
renamed ATOM file lost the second level.

Since #74 a new store decides **by size alone**: new chunks of every file of at least `--sub-min-file` (default
**1 MiB**) get sub-chunks. No name, extension or format is read (ADR-0009). `pkg/layout` is used only by reports
(`dedup-report` groups, `inspect`) and as a pairing hint in `diff`.

## Candidates and measurements

Measured 2026-10-30 on the lab node (ATOM build set, HF checkpoint and RNGD artifacts on local disk; `nice -n 19`,
16 threads, shared node; n = 1 per configuration, since stored sizes are deterministic for a given push order). Raw data:
[`data/sub-chunk-policy-20261030.json`](data/sub-chunk-policy-20261030.json).

- **kind**: the old default, file kinds by name.
- **size ≥ 1 MiB** (chosen) and **size ≥ 64 MiB**.
- **probe** (experiment, commit ec9bc0f, not merged): sub-chunk a file's first 64 new chunks; continue only if at
  least 10 % of their sub-chunk bytes were already stored.

| store after pushing | kind | size ≥ 1 MiB | probe |
|---|---|---|---|
| ATOM set (263.9 GB, 19 files) | **16.70 GB** (metadata 1.08 GB), 110 s | 16.78 GB (+0.5 %; 1.16 GB), 120 s | 26.92 GB (+61 %; 0.69 GB), 172 s |
| ATOM set, then HF checkpoint | 27.44 GB | 28.06 GB | 37.66 GB |
| HF checkpoint alone (16.07 GB) | 10.73 GB (metadata 0.03 GB), 38 s | 11.28 GB (+5.1 %; 0.56 GB), 122 s | 10.74 GB, 104 s¹ |
| RNGD pfbatch, preset, coarse, fine (32.4 GB) | 10.79 GB, pfbatch push 38 s | 11.53 GB (+6.9 %), 113 s | 10.78 GB, 37 s |

| switch: bytes a node fetches | kind | size ≥ 1 MiB | size ≥ 64 MiB | probe |
|---|---|---|---|---|
| ATOM decode B2 → B8 (15.2 GB, `_lm` variants) | 3.57 MB | 3.57 MB | — | 3.58 MB |
| ATOM decode B2 (no lm_head) → B8 | 694 MB | 694 MB | — | 697 MB |
| RNGD pfbatch → preset | 4.33 MB | **0.65 MB** | 4.33 MB | 2.59 MB |

Probe sizes are from a push with one worker (deterministic order); with 16 workers the ATOM set stores 27.00 GB and
RNGD 10.78 GB, and the push times shown for the ATOM set and RNGD are from that run. ¹ one worker. The binary from
`main` with its defaults stores the ATOM set in 16.70 GB, the same as **kind**.

What the numbers say:

- **Size** never loses dedup. On the ATOM set it equals the kind policy (it also sub-chunks `tables.npz`, +77 MB),
  and it finds the furiosa-llm graph bundles' rearranged bytes: the RNGD switch fetches 0.65 MB instead of 4.33 MB.
  Its cost is metadata where content is new and not rearranged, as in a checkpoint pushed for the first time:
  +0.53 GB (3.3 % of 16 GB) of indexes and composite tables, about 3× push time for that new content, and a locator of
  ~230 MB instead of ~12 MB for every artifact that references the sub-chunked checkpoint (RNGD preset).
- **Probe** keeps checkpoints cheap but loses 10.2 GB on the ATOM set. The first file of a family finds nothing
  stored, so it is not sub-chunked, and later files cannot match its bytes at 4 KiB. Which file comes first depends
  on push order and concurrency, so the loss does too.
- **64 MiB** costs as much as 1 MiB (checkpoint shards are GBs) and misses the 13–30 MB bundles.

Bytes alone cannot tell the first copy of rearranged weights from a checkpoint. A policy that skips sub-chunks for
new content (probe) therefore trades a bounded metadata cost for an unbounded dedup loss. The size policy is the
vendor-neutral default: the worst case is a few percent of metadata on new checkpoints. Operators who know a store
holds only checkpoints can create it with `--sub 0`; `--sub-kinds` still gives the old behaviour.

Follow-up: locators list the sub-chunks of every composite an artifact references, so referenced checkpoints make
every artifact's locator large. Referencing composites instead would remove most of the size policy's cost.

## Compatibility

- **Existing stores** keep what their config records. A store with `sub_kinds` keeps deciding by kind
  (`TestSubPolicyIsStoredWithTheStore`), and the binary from this branch reads and pushes to stores made by `main`.
- **New stores** record `"sub_min_file": 1048576` in `config/store`; `onggi ls` shows the policy.
- **Formats** are unchanged: manifests (including `files[].kind`), chunk lists, locators, indexes, composite tables.
  The binary from `main` pulled an artifact from a store created by this branch, byte-identical.
- **Old binaries pushing to a new store** ignore `sub_min_file` (unknown JSON field) and, since the store records no
  `sub_kinds`, sub-chunk every file, small ones included. The result is valid and readable by every version; it only
  costs more metadata for files under 1 MiB.
- **CLI:** `--sub-min-file SIZE` (default 1MiB); `--sub-kinds LIST|all` overrides it (the behaviour before #74);
  both only when creating a store. `--sub 0` still turns the second level off.

## Placement classes (`onggi dedup-report --classify`)

`dedup-report --classify [--reference PATH]...` also reports, per file, how the bytes it shares can be found, from
chunk hashes alone. "Elsewhere" means another file of the set or a reference file. Each coarse 64 KiB chunk is split
into fine chunks (`--fine 4KiB`), as the store's second level does. The rules apply in this order:

| class | rule (default threshold) | meaning |
|---|---|---|
| `referenced` | ≥ `--ref-share` 0.95 of the bytes are in the references at coarse chunks | a copy of the checkpoint |
| `embedded-rearranged` | fine share − coarse share ≥ `--rearranged-gap` 0.25 | shared bytes in runs shorter than a chunk: needs the second level |
| `embedded-shifted` | coarse share ≥ `--shifted-share` 0.5 | shared bytes copied whole, at any offset |
| `new` | otherwise | |

The JSON gains `files[].class`, `files[].shares` (`ref`, `coarse`, `fine`) and a `classify` block with the thresholds.
Fine-chunk keys are the first 8 bytes of SHA-256; a collision only moves bytes between shares.

On the lab node, with the HF checkpoint as reference:

- **ATOM set** (77 s, 1.8 GB RSS):
  - 13 decode graphs, `llama8b_S256_fp16` and prefill C16 are `embedded-shifted` (coarse 98.6–99.8 %).
  - Prefill C128, C256 and S512_C512 are `embedded-rearranged` (coarse 39–45 %, fine 87–90 %).
  - `tables.npz` is `new`.
  - No ATOM file shares bytes with the bf16 checkpoint (ref 0.0).
- **RNGD artifacts** (23 s, 0.45 GB RSS):
  - Param shards 1–3 are `referenced` (ref 1.000).
  - Shard 4 holds 5.5 % that is not in the checkpoint (ref 0.9455), so it is `embedded-shifted`.
  - The four `binary_bundle.zip` are `embedded-rearranged` (coarse 30–58 %, fine 88–96 %). This is why the size
    policy, which also sub-chunks them, cuts the pfbatch → preset switch from 4.33 to 0.65 MB.
  - `artifact.json` is `new`.

Identical small files (configs, tokenizer) show as `embedded-shifted`. The classes describe where shared bytes lie,
not what a file is, and they mean most for weight-bearing files. A TensorRT-LLM engine, 99.8 % of whose bytes are in
its checkpoint (#56), would be `referenced` against the checkpoint and `embedded-shifted` against sibling engines
without one. The engines live on the lab A100 host, not on this node, and were not re-measured here.
