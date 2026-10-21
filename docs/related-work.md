# Related work

Surveyed 2026-10-21 · [한국어](ko/related-work.md)

Onggi combines published techniques. This page lists the closest prior work for each part, what onggi does **not**
claim, and where it differs. Licence rules for reusing code are in [ADR-0009](adr/0009-clean-room-and-vendor-formats.md).

## 1. Content-defined chunking, packs, only-missing transfer

| Work | Kind | What it already does |
|---|---|---|
| LBFS, Muthitacharoen et al., SOSP 2001 | paper | Rabin-fingerprint chunking to send only chunks the receiver lacks |
| Data Domain, Zhu et al., FAST 2008 | paper | segment dedup, containers with a per-container index |
| FastCDC, Xia et al., USENIX ATC 2016; IEEE TPDS 2020 | paper | gear hash, cut-point skipping, normalized chunking (onggi's chunker implements this) |
| [Hugging Face Xet](https://huggingface.co/docs/xet/index) ([xet-core](https://github.com/huggingface/xet-core), Apache-2.0) | system | gear-hash chunks of ~64 KiB, **xorbs ≤ 64 MiB**, shards mapping files to chunk ranges, global dedup, upload of missing chunks only, per-chunk LZ4 or byte grouping + LZ4 |
| [casync](https://github.com/systemd/casync) / [desync](https://github.com/folbricht/desync) | system | chunk index + chunk store, HTTP/S3 pull of missing chunks, local files as seeds |
| [restic](https://github.com/restic/restic), [Borg](https://github.com/borgbackup/borg) | system | CDC, packs with a per-pack index, many backends |
| [zsync](http://zsync.moria.org.uk), [zchunk](https://github.com/zchunk/zchunk) | system | range download of the parts a client lacks |
| ZipLLM, Wang et al., NSDI 2026; TStore, arXiv 2604.17104 | paper | tensor-level dedup of model hubs; argue it beats CDC for framework checkpoints |

**Not claimed:** the chunker, packs, indexes, manifests, refs with compare-and-swap, only-missing upload, ranged pull.
Onggi's chunk boundaries do not interoperate with Xet's (different gear table); this is an engineering choice, not a
contribution.

## 2. Lossless compression of bf16/fp16 weights (#8)

| Work | What it already does |
|---|---|
| HDF5 shuffle filter, [Blosc](https://github.com/Blosc/c-blosc2) shuffle/bitshuffle | byte or bit planes before a general codec, in use for decades |
| ZipNN, Hershcovitch et al., IEEE CLOUD 2025 ([code](https://github.com/zipnn/zipnn), MIT) | byte grouping, Huffman-coded exponents; ~33 % smaller bf16 models |
| DFloat11, Zhang et al., NeurIPS 2025 ([code](https://github.com/LeanModels/DFloat11), Apache-2.0) | Huffman-coded bf16 exponent, raw sign and mantissa, GPU decoder; ~30 % |
| NeuZip, NeurIPS 2024; Huff-LLM, arXiv 2502.00922; ECF8, ICLR 2026 | exponent entropy coding (NeuZip code is non-commercial; see ADR-0009) |
| Xet `ByteGrouping4LZ4` | byte grouping per CDC chunk, after dedup |

**Not claimed:** byte planes, exponent coding, or compressing after dedup. Onggi's measurements on the lab node
(bf16 Llama-3.1-8B, 32 MB samples per tensor): zlib on raw bytes 1.26×, on split byte planes 1.43×, exponent-entropy
bound 1.50–1.51×. These match the published figures. Any onggi work here is engineering (speed, integration with
packs), not a new algorithm.

## 3. Delta transfer between similar files (#9)

| Work | What it already does |
|---|---|
| rsync (1996), xdelta3 / VCDIFF (RFC 3284) | rolling-checksum matching; reference-based delta encoding |
| Shilane et al., FAST 2012; Ddelta, Odess, DeepSketch | similarity detection plus delta after dedup |
| ZipLLM / BitX, TStore | lossless XOR deltas between related models, family clustering |
| BitDelta (NeurIPS 2024), DeltaZip (EuroSys 2025), ExCP (ICML 2024), Delta-DNN (ICPP 2020) | lossy deltas between fine-tunes or checkpoints |

**Not claimed:** fetching missing chunks, reference-based deltas, or similarity sketches.

## 4. Delivery on Kubernetes

| Work | What it already does |
|---|---|
| [Dragonfly](https://github.com/dragonflyoss/dragonfly) / [Nydus](https://github.com/dragonflyoss/nydus), Kraken, Spegel | peer-to-peer distribution of images and models, chunk dedup across images |
| [ModelPack](https://github.com/modelpack/model-spec), KServe Modelcar / LocalModelCache, Kubernetes image volumes (KEP-4639) | models as OCI artifacts, node-local caches, read-only artifact volumes |
| [Fluid](https://github.com/fluid-cloudnative/fluid), JuiceFS | dataset and model caching and prefetch |
| ServerlessLLM (OSDI 2024), Run:ai Model Streamer, fastsafetensors | fast loading, multi-tier caches, locality-aware placement |
| [optimum-neuron cache](https://huggingface.co/aws-neuron/optimum-neuron-cache) | compiled accelerator artifacts cached by graph and flag hash, as whole files |

**Not claimed:** P2P distribution, OCI packaging, node caches or CSI delivery as such.

## 5. Where onggi differs

1. **Compiled NPU artifacts as the subject.** We found no published measurement of redundancy across compile variants
   of NPU-compiled LLMs. Onggi's measurements ([artifact-formats.md](artifact-formats.md)):
   - The ATOM Llama-3.1-8B build set goes from 262 GB to 44.7 GB.
   - ATOM decode variants share 98.5–99.7 %; prefill variants share 29–37 %.
   - RNGD weight files equal the HF source byte for byte.
   - RNGD graph sets overlap per bucket.

   The closest system, the optimum-neuron cache, stores whole files by hash.
2. **The compile matrix as the family axis.** Variants differ by compiler version, flags, bucket, batch and parallel
   split rather than by fine-tuning. Placement, prefetch and GC use that structure.
3. **One store for checkpoints and compiled outputs.** The store works from published metadata and from bytes the user
   produced. It does not decode vendor formats (ADR-0009).
4. **Kubernetes + llm-d integration for heterogeneous NPUs.** This covers the compile farm, the node agent and CSI, and
   artifact-aware placement.

A paper should lead with (1) as a measurement study plus a system, use Xet, ZipLLM/TStore, ZipNN/DFloat11, zstd
long-range mode and image volumes / Dragonfly as baselines, and present (2)–(4) as the system design.
