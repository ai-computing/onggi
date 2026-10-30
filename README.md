# Onggi

**Content-addressed store for model weights and accelerator-compiled artifacts on Kubernetes + llm-d.**

English · [한국어](README.ko.md)

> Status: **P2.** The design is in [docs/design.md](docs/design.md). `onggi push` / `pull` store and restore artifacts
> on a POSIX mount or an S3 server, with GC, a node cache, peers and delta apply; the CSI driver
> ([deploy/csi](deploy/csi/README.md)) serves them as read-only volumes on Kubernetes. The controller (compile farm,
> prefetch) follows in P3. Measurements against common methods: [docs/evaluation.md](docs/evaluation.md).

Live delta comparison: [reuse target metadata planning](docs/diff-plan-2026-10-30.md)
reduced measured CLI time by 8–14% on the 8B sparse-update fixtures, with identical range output.

NPUs compile models ahead of time, one artifact per model × SDK × device × sequence bucket × batch × options. On our
RNGD + ATOM node, one Llama-3.1-8B build set for Rebellions ATOM takes **262 GB** because every graph embeds the
weights, and FuriosaAI RNGD keeps three copies of weights that are byte-identical to the HF checkpoint. Content-defined
chunking finds 99.3 % shared bytes between ATOM variants that fixed-size blocks miss entirely
([measurements](docs/artifact-formats.md)).

Onggi (옹기, the Korean jar that stores and ferments food) keeps each distinct byte once and hands restored artifacts
to engines as plain files. The CSI plugin restores them on demand; placement-driven prefetch remains planned.

<p align="center"><img src="docs/assets/architecture.svg" width="100%" alt="Onggi architecture: users act through the Kubernetes API; onggi-controller runs the compile farm, prefetch, GC and a webhook; a durable tier behind pluggable fs, s3 and oci backends; a cache tier of node agents that fetch missing chunks, share them peer to peer and mount artifacts into engine pods; integration points with rm-core, rm-accel, rm-controller, llm-d, npu-train, lmd-top and Optimus Prime"></p>

**Target architecture:** ① a compile Job (or `onggi push`) chunks an artifact and uploads only the chunks the durable
tier does not have, then ② commits a Ref in the Kubernetes API. ③ When a pod mounts the artifact, the node's agent
fetches the missing chunks from peers or the durable tier, verifies every hash and hands the engine plain files.
The Kubernetes Ref API, compile controller and fetches triggered by ai-rm/llm-d placement or scale-up signals are future work.

## What it does

| | |
|---|---|
| **Store once** | content-defined chunks, manifests per artifact, weights stored once per weight layout |
| **Reuse node data** | node cache and hash-checked peer transfer; placement/autoscaling prefetch is planned |
| **Deliver as files** | CSI volume: kubelet mounts a ready artifact; vLLM, vllm-rbln, furiosa-llm, rngd-pd and Optimus Prime need no changes |
| **Build declaratively (planned)** | `ArtifactSet` CRDs and a compile farm; today, publish existing builds with `onggi push` |
| **Stay multi-node** | durable tier on an existing erasure-coded object store; caches survive node loss by design |

## Prepared variants and NPU delta loading

Onggi restores precompiled variants and weights, including their unchanged chunks, as ordinary files. A serving
runtime such as [rngd-pd](https://github.com/ai-computing/rngd-pd) selects a compatible prepared batch/sequence
variant, drains active requests when required, and performs any supported device-memory update. Onggi's file
delta does not itself update NPU memory or establish that two compiled graphs can share resident weights.

Record the source checkpoint identity, compiler/SDK, device, weight layout and supported batch/sequence shapes
with each build. Changing batch or sequence length still requires a compatible precompiled graph. RNGD external
weights and ATOM device-input weights have different runtime capabilities; ATOM graphs with embedded weights
require their corresponding compiled artifacts. Automated compatibility selection, prewarm and placement hooks
remain integration work; see [integration](docs/integration.md).

The [resident-delta integration record](docs/resident-delta.md) connects actual
Onggi ranges to RNGD row updates: 64 projection rows reduced logical upload
117 MB → 1.84 MB, and eight LM rows reduced 1.05 GB → 64 KiB, with 768 generated
tokens equal to fresh loads. Those measurements use SDK 2026.2; the record also
documents the SDK 2026.4 preflight and the separate
[native cache-loader migration](https://github.com/ai-computing/rngd-pd/pull/31).

## Quick start

```bash
go build -o onggi ./cmd/onggi            # Go 1.24+
./onggi dedup-report --pairs 6 --fixed-block 1MiB <model store paths>...
./onggi dedup-report --json <paths>... > report.json
```

It walks the paths (symlinks are not followed), sorts files by kind (HF source, RNGD params/graphs/bundles, ATOM
decode/prefill `.rbln`) so source checkpoints come first, chunks files in parallel at content-defined boundaries
(FastCDC, 16/64/256 KiB by default) and prints, per kind, the logical size, the distinct bytes and the bytes each
kind adds on top of the earlier ones. `--fixed-block` repeats the measurement with fixed-size blocks for comparison;
`--pairs N` prints which share of each of the N largest files occurs in each other.

Store and restore an artifact (only chunks the store lacks are uploaded and are compressed per chunk; every chunk is
hash-checked on the way back):

```bash
export ONGGI_STORE=/mnt/models/onggi        # any POSIX mount: local disk, NFS, SMB, CephFS, ...
# or an S3 server (Ceph RGW, SeaweedFS, Garage, VersityGW, cloud); the bucket must exist
export ONGGI_STORE='s3://models/onggi?endpoint=s3.lab.example:8333&insecure=1' AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
./onggi push --ref llama-3.1-8b@atom/pd <build dir>
./onggi pull llama-3.1-8b@atom/pd <restore dir>
./onggi ls; ./onggi inspect llama-3.1-8b@atom/pd
./onggi pull --cache /var/lib/onggi/cache llama-3.1-8b@atom/pd-b8 <dir>   # node cache: read only what the node lacks
./onggi peer serve --cache /var/lib/onggi/cache --listen :7070            # share this node's cache with others
./onggi pull --cache /var/lib/onggi/cache --peers http://node-a:7070 <ref> <dir>   # ask peers before the store
```

On the lab node, the 264 GB ATOM Llama-3.1-8B build set is stored in **15.9 GB** (push 3 min, pull 5 min,
byte-identical). Two steps get it there. Two-level chunking re-splits new ATOM chunks into ~4 KiB sub-chunks, so prefill
variants that hold the same weights in a different order dedup (24.9 GB new instead of 46.8 GB). Per-chunk compression
then takes 1.6× off. `--compress none` and `--sub 0` turn the two off. New chunks of sub-chunked files are compressed together in frames
of up to 255 KiB where that is smaller (#52; `--frame 0` turns it off).

With `--cache DIR` a pull reads chunks the node already holds from a local cache store and adds what it fetches. On
the lab node, after one ATOM decode variant (10.2 GB read), a second one read 28 MB from the store (0.3 %), a prefill
build 1.9 GB, and a repeat 0 B; every restore was byte-identical. The CSI plugin evicts idle artifacts and collects the node cache on its own (#33).
With `--peers` a pull asks other nodes' caches (`onggi peer serve`) for the chunks it lacks before the store. Each
chunk goes to one holder, chosen by rendezvous hashing, and is hash-checked; whatever a peer does not deliver comes
from the store. In the lab cluster a CPU node pulled the decode variant (10.2 GB stored) from an S3 store with the
NPU node's warm cache as its peer: 0 B from the store, 10.2 GB from the peer, byte-identical (#34).
On Kubernetes, the CSI driver ([deploy/csi](deploy/csi/README.md)) mounts a ref as a read-only volume: the node
materializes each artifact once, through the node cache, and pods with the same ref share that copy. On the lab
cluster a second decode variant read 28 MB from the store. An init container into the pod's emptyDir also works
([deploy/k8s](deploy/k8s/README.md)).

Garbage: `onggi rm-ref` drops a ref, and `onggi gc` reclaims what no ref reaches (ADR-0005). A run records the
unreferenced objects; a run at least `--grace` (default 24 h) later deletes the ones that are still unreferenced, unless
a push holds a lease. Packs that are mostly dead are repacked first (`--repack 0.25`). On the lab node, dropping the full
ATOM set and one decode variant and keeping two variants took the packs from 15.9 GB to 12.2 GB (live: 12.1 GB) in
four runs, and both remaining artifacts pulled byte-identical.
A node cache has no refs: `onggi gc --store CACHE --roots-store S --keep-used 168h` keeps what the artifacts pulled
through it within that period need (pulls record their use) and evicts the rest. On the lab node a cache holding three
variants (12.2 GB) shrank to the one still in use (10.2 GB); pulling it again read 0 B from the store.

`onggi diff --store S <base> <ref>` shows what turning a node's copy of one artifact into another takes, by stored
chunks and without parsing any format (#38): bytes at the same offset (can stay), bytes the base holds elsewhere
(a local copy), and new bytes (fetched). On the lab store:

| base → new | same offset | elsewhere in base | new | to fetch | changed runs |
|---|---|---|---|---|---|
| decode B2 → decode B8 (15.2 GB) | 98.6 %, all in whole 4 KiB blocks | 0 | 1.4 % | 28.3 MB | 3, largest 218 MB |
| decode B2 → prefill C128 (14.0 GB) | 0 | 79.4 % | 20.6 % | 1.9 GB | 1 |

Decode variants differ in one contiguous region, so a node updates a copy in place instead of rewriting 15 GB:
`onggi pull --base REF --base-dir DIR` turns an artifact restored in DIR into the new one and writes only what differs
(#41), and the CSI plugin does this with artifacts no pod uses. On the lab node decode B8 came from a B2 copy in 5.7 s
(8.9 s through the node cache) instead of 73.8 s, byte-identical. Prefill and decode hold the same weights in a
different order.

Memory: a pull reads only the artifact's locator, not the store's whole index. For that set it peaks at 3.6 GB RSS by
default and stays under 1.9 GB with `GOMEMLIMIT=1800MiB` (Go's soft memory limit), which node agents should set. Push
holds the store's index (compact arrays) for dedup and peaks at 1.9 GiB by default (push 2 min 24 s).

## Documents

| | |
|---|---|
| [Design](docs/design.md) | problem, goals, quality attributes, principles, architecture, flows, multi-node storage, roadmap |
| [Artifact formats](docs/artifact-formats.md) | what RNGD and ATOM artifacts contain and how much they share (measured) |
| [Integration](docs/integration.md) | how onggi connects to ai-rm components, lmd-top and llm-d |
| [Related work](docs/related-work.md) | prior work per component, what onggi does not claim, where it differs |
| [ADRs](docs/adr/README.md) | architecture decisions |

## Part of ai-rm

Onggi is a component of [ai-rm](https://github.com/ai-computing/ai-rm), the heterogeneous AI resource management stack.
Its [rm-core](https://github.com/ai-computing/rm-core) artifact contract and
[lmd-top](https://github.com/ai-computing/lmd-top) operator-console integration are planned; the current interfaces are the CLI and CSI volumes.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). License: [Apache-2.0](LICENSE).
