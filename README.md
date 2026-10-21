# Onggi

**Content-addressed store for model weights and accelerator-compiled artifacts on Kubernetes + llm-d.**

English · [한국어](README.ko.md)

> Status: **P1.** The design is in [docs/design.md](docs/design.md). `onggi dedup-report` measures what a store would
> deduplicate; `onggi push` / `pull` store and restore artifacts on a POSIX mount or an S3 server. The cluster
> components (node agent, CSI driver, controller) follow in P2–P3.

NPUs compile models ahead of time, one artifact per model × SDK × device × sequence bucket × batch × options. On our
RNGD + ATOM node, one Llama-3.1-8B build set for Rebellions ATOM takes **262 GB** because every graph embeds the
weights, and FuriosaAI RNGD keeps three copies of weights that are byte-identical to the HF checkpoint. Content-defined
chunking finds 99.3 % shared bytes between ATOM variants that fixed-size blocks miss entirely
([measurements](docs/artifact-formats.md)).

Onggi (옹기, the Korean jar that stores and ferments food) keeps each distinct byte once, moves artifacts to the nodes
that need them before a pod asks, and hands them to any engine as plain files.

<p align="center"><img src="docs/assets/architecture.svg" width="100%" alt="Onggi architecture: users act through the Kubernetes API; onggi-controller runs the compile farm, prefetch, GC and a webhook; a durable tier behind pluggable fs, s3 and oci backends; a cache tier of node agents that fetch missing chunks, share them peer to peer and mount artifacts into engine pods; integration points with rm-core, rm-accel, rm-controller, llm-d, npu-train, lmd-top and Optimus Prime"></p>

**How it fits together:** ① a compile Job (or `onggi push`) chunks an artifact and uploads only the chunks the durable
tier does not have, then ② commits a Ref in the Kubernetes API. ③ When a pod mounts the artifact, the node's agent
fetches the missing chunks from peers or the durable tier, verifies every hash and hands the engine plain files.
Placement decisions and scale-up signals from ai-rm and llm-d start the fetch before the pod exists.

## What it does

| | |
|---|---|
| **Store once** | content-defined chunks, manifests per artifact, weights stored once per weight layout |
| **Place ahead** | per-node cache tier with rendezvous (HRW) placement and peer-to-peer transfer; prefetch from placement and autoscaling signals |
| **Deliver as files** | CSI volume: kubelet mounts a ready artifact; vLLM, vllm-rbln, furiosa-llm, rngd-pd and Optimus Prime need no changes |
| **Build declaratively** | `ArtifactSet` CRDs drive a compile farm; results are deduplicated on publish |
| **Stay multi-node** | durable tier on an existing erasure-coded object store; caches survive node loss by design |

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
```

On the lab node, the 264 GB ATOM Llama-3.1-8B build set is stored in **15.9 GB** (push 3 min, pull 5 min,
byte-identical). Two steps get it there. Two-level chunking re-splits new ATOM chunks into ~4 KiB sub-chunks, so prefill
variants that hold the same weights in a different order dedup (24.9 GB new instead of 46.8 GB). Per-chunk compression
then takes 1.6× off. `--compress none` and `--sub 0` turn the two off.

## Documents

| | |
|---|---|
| [Design](docs/design.md) | problem, goals, quality attributes, principles, architecture, flows, multi-node storage, roadmap |
| [Artifact formats](docs/artifact-formats.md) | what RNGD and ATOM artifacts contain and how much they share (measured) |
| [Integration](docs/integration.md) | how onggi connects to ai-rm components, lmd-top and llm-d |
| [Related work](docs/related-work.md) | prior work per component, what onggi does not claim, where it differs |
| [ADRs](docs/adr/README.md) | architecture decisions |

## Part of ai-rm

Onggi is a component of [ai-rm](https://github.com/ai-computing/ai-rm), the heterogeneous AI resource management stack,
and connects through [rm-core](https://github.com/ai-computing/rm-core) contracts. [lmd-top](https://github.com/ai-computing/lmd-top)
is its operator console.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). License: [Apache-2.0](LICENSE).
