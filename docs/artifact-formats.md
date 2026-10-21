# Compiled artifact formats: what is in them and how much is shared

Measured 2026-10-21 on the lab's RNGD + ATOM node (read-only) · [한국어](ko/artifact-formats.md)

## FuriosaAI RNGD (furiosa-llm 2026.x)

| Item | Finding |
|---|---|
| Graphs | `.edf` files, a few KB to 16 MB each, named by a content hash (sha256 or a 24-hex prefix). Binary, self-describing node lists. |
| Weight references | `.w` files (≈ 2.5 KB zip with a `data.pkl`) point at weights; they carry no tensor data. |
| Weights | `param_files/params-<model>-<layers>-W16A16KV16-shard_size=…-<hash>.safetensors/`: the HF checkpoint re-sharded into four files. The bytes are **identical** to the HF source (64-byte probes at four offsets all found; content-defined chunking: 100 % shared). |
| Artifact bundle | `artifact.json` (compiler and furiosa-llm versions, tensor/pipeline parallel sizes, pipeline metadata) + `binary_bundle.zip` (≈ 90 `.edf`, 15 MB) + tokenizer and config files. A Llama-3.1-8B artifact without weights: 34–53 MB. |
| Node footprint | 531 `.edf` files, 0.28 GB in total; the same converted Llama-3.1-8B weights stored three times (48 GB) across a cache and two artifact directories. |

The graphs are already small and content-addressed. The waste is copies of a converted weight set that equals the
source checkpoint.

## Rebellions ATOM (optimum-rbln / rebel-compiler)

| Item | Finding |
|---|---|
| Format | one `.rbln` binary per compiled graph, header `RBLN2024`; `rbln_config.json` beside it. |
| Weights | **embedded** in each `.rbln`, in a compiler-specific layout (0 % of chunks shared with the HF source). |
| Sizes (Llama-3.1-8B) | 13.4–14.5 GB per decode or prefill graph, about the size of the bf16 weights. |
| One build set | 18 files (decode S256/S512 × B1/B2/B4/B8, with and without lm_head; prefill S256/S512 with several chunk sizes): **262 GB** (246 GiB). |
| Node footprint | 215 `.rbln` files, **979 GB**. |
| Vendor-style build | a separate build had `prefill.rbln` 15 GB with `decoder_batch_1/8.rbln` ≈ 1 GB each, which suggests weight sharing inside one compile. |

## How much is shared

Content-defined chunking (8-byte window hash, 64 KiB average, 16 KiB minimum, 256 KiB maximum; BLAKE2b-128 per chunk)
over a 121 GB sample: the HF Llama-3.1-8B source (4 files), one RNGD param copy (4 files) and six ATOM files.

| Pair | Share of the first file's bytes found in the second |
|---|---:|
| HF source ↔ RNGD params | 100 % |
| ATOM decode B1 ↔ B2 | 99.3 % |
| ATOM decode B1 ↔ B4 | 99.3 % |
| ATOM decode S256 ↔ S512 | 99.3 % |
| ATOM decode with ↔ without lm_head | 92.4 % |
| ATOM decode ↔ ATOM prefill | 27.5 % |
| HF source ↔ ATOM decode | 0 % |

| Group | Logical | Unique after dedup |
|---|---:|---:|
| HF source | 16.1 GB | 16.1 GB |
| RNGD params | 16.1 GB | 0.1 GB |
| ATOM (6 files) | 89.0 GB | 25.7 GB |
| **Total** | **121.2 GB** | **41.9 GB (2.9×)** |

Each further ATOM decode variant adds 0.05–0.19 GB; the first prefill adds 10.1 GB (a second weight layout).
The estimate drawn from these shares for the whole build set turned out too low; see the measurement below.

Fixed-size block hashing misses the ATOM sharing entirely: 1 MiB blocks and 4 KiB blocks both found 0 % between batch
variants and between decode and prefill, while 48-byte probes from one file were found in the other. The shared bytes
sit at offsets that differ by amounts that are not block multiples. **Content-defined chunking is required.**

Compression adds little: zlib level 6 on an 8 MB weight region of an `.rbln` gives 1.29×.

## Measured with `onggi dedup-report` (P0)

The same chunking in Go (FastCDC, 16/64/256 KiB, SHA-256) on the node, commit `033814e`. Each pass reads every file once:

| Run | Files | Logical | Distinct | Ratio | Time |
|---|--:|--:|--:|--:|--:|
| The 121 GB sample above (reproduction) | 14 | 121.2 GB | 42.6 GB | 2.85× | 42 s |
| One Llama-3.1-8B set: HF source, RNGD params and graphs, the full ATOM build set | 342 | 330.0 GB | 64.0 GB | 5.15× | 2 min |
| The whole node's model stores (files ≥ 1 MiB) | 1,642 | 1,728.7 GB | 938.6 GB | 1.84× | — |

The full ATOM Llama-3.1-8B build set is **262 GB** (246 GiB; 18 files). By kind, after the HF source:

| Kind | Files | Logical | Distinct within the kind | New after earlier kinds |
|---|--:|--:|--:|--:|
| ATOM decode | 13 | 191.2 GB | 16.8 GB | 16.8 GB |
| ATOM prefill | 4 | 56.5 GB | 40.7 GB | 27.8 GB |
| ATOM other (`llama8b_S256_fp16`) | 1 | 14.1 GB | 14.1 GB | 0.1 GB |
| **ATOM total** | 18 | **261.8 GB** | | **44.7 GB (5.9×)** |
| RNGD params (3 copies) | 12 | 48.4 GB | 16.1 GB | 0.07 GB |

**Correction:** the estimate made from the sample (28–30 GB, ≈ 8×) was too low. Decode variants behave as the sample
said (each adds 0.05–0.2 GB), but **prefill builds with different chunk sizes use different weight layouts**: the
four prefill files (C16, C128, C256, C512) share only 29–37 % of their bytes with each other, so each adds about
10 GB. Prefill chunk size, not sequence length or batch, is the expensive dimension of an ATOM build set.

Whole node, by kind: HF safetensors 811.6 GB → 742.6 GB (copies of the same checkpoints in several directories);
ATOM `.rbln` 761.2 GB → 175.1 GB new; RNGD params 48.4 GB → 0.1 GB new. 524 symlinks (hub snapshot links) were not
followed. The run covered the node's model directories, not every `.rbln` on the disk (979 GB in total).

Fixed 1 MiB blocks on the 330 GB set: 150.0 GB (2.20×), against 64.0 GB content-defined.

## Consequences for the design

1. Weights are the unit that matters. Store them once per **weight layout** (`hf`/`rngd`, `atom-decode`,
   `atom-prefill`), and graphs separately.
2. Chunk boundaries must be content-defined (ADR-0001).
3. Cross-vendor dedup is not possible where a compiler re-lays weights out; dedup within a vendor and layout is.
4. The RNGD layout equals the HF source, so the RNGD path can reference the source checkpoint directly.
5. Each ATOM prefill chunk size costs a full weight layout; semantic compaction should look there first.

## Method

The probes ran as read-only scripts on the node: file listings and sizes; magic bytes and headers; 48/64-byte
substring searches; fixed-block and content-defined chunk hashing with Python and NumPy. `onggi dedup-report` (P0) now
reproduces them with one command.
