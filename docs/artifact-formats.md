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
| One build set | 18 files (decode S256/S512 × B1/B2/B4/B8, with and without lm_head; prefill S256/S512 with several chunk sizes): **246 GB**. |
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
Applied to the 18-file build set, this gives an estimated 28–30 GB for 246 GB (≈ 8×). The estimate is from the
sample's per-file shares, not a run over all 18 files.

Fixed-size block hashing misses the ATOM sharing entirely: 1 MiB blocks and 4 KiB blocks both found 0 % between batch
variants and between decode and prefill, while 48-byte probes from one file were found in the other. The shared bytes
sit at offsets that differ by amounts that are not block multiples. **Content-defined chunking is required.**

Compression adds little: zlib level 6 on an 8 MB weight region of an `.rbln` gives 1.29×.

## Consequences for the design

1. Weights are the unit that matters. Store them once per **weight layout** (`hf`/`rngd`, `atom-decode`,
   `atom-prefill`), and graphs separately.
2. Chunk boundaries must be content-defined (ADR-0001).
3. Cross-vendor dedup is not possible where a compiler re-lays weights out; dedup within a vendor and layout is.
4. The RNGD layout equals the HF source, so the RNGD path can reference the source checkpoint directly.

## Method

The probes ran as read-only scripts on the node: file listings and sizes; magic bytes and headers; 48/64-byte
substring searches; fixed-block and content-defined chunk hashing with Python and NumPy. They will be part of
`onggi dedup-report` (P0) so the numbers can be reproduced.
