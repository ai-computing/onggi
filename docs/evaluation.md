# Evaluation

Measured 2026-10-22 on the lab's RNGD + ATOM node with [`bench/baselines.sh`](../bench/baselines.sh) (#51), over one
Rebellions ATOM Llama-3.1-8B build set: 18 files, 263.9 GB (decode and prefill graphs for several batch sizes and
sequence buckets, each embedding the weights).

## Storing the set

| method | stored | vs raw | time |
|---|---|---|---|
| raw | 263.9 GB | 1.00× | — |
| OCI-style layers: whole-file dedup, zstd -3 per file | 199.2 GB | 1.33× | 27.6 min |
| one tar, zstd -3 `--long=31` (2 GiB window) | 199.1 GB | 1.33× | 28.3 min |
| fixed-block dedup, 1 MiB | 116.4 GB | 2.27× | 26 s |
| fixed-block dedup, 64 KiB | 113.4 GB | 2.33× | 27 s |
| FastCDC 64 KiB dedup | 46.8 GB | 5.64× | — |
| Xet-style: FastCDC 64 KiB + per-chunk compression (`onggi --sub 0`) | 29.5 GB | 8.95× | 1.0 min |
| onggi before #52: two-level chunking + per-chunk codecs | 17.0 GB | 15.6× | 1.3 min |
| **onggi**: + compression frames (#52) | **16.7 GB** | **15.8×** | 1.5 min |

Whole-file methods find nothing to share: every file differs. A long-range window of 2 GiB cannot reach the copy of
the weights in the previous 15 GB file. Fixed blocks lose the weights as soon as a header shifts them. CDC finds the
weights; sub-chunks also find the ones prefill graphs hold in a different order (#15). Sizes include each store's
indexes and manifests.

Xet itself needs the Hugging Face service; the Xet-style row uses the same idea (64 KiB CDC, per-chunk compression)
inside onggi. The compression times run under `nice -n 19` on a shared node.

## Switching a node from decode B2 to decode B8

Bytes that cross the network when a node that holds B2 needs B8 (15.2 GB):

| method | transferred |
|---|---|
| OCI layer (zstd -3) | 11.46 GB |
| Xet-style CDC + per-chunk compression | 6.1 MB |
| onggi before #52 (each ~4 KiB sub-chunk compressed alone) | 28.3 MB |
| **onggi** with compression frames (#52) | **3.6 MB** |

The 218 MB that differ compress well. onggi splits new chunks of ATOM files into ~4 KiB sub-chunks for dedup; compressed
one by one they sent 28.3 MB. Since #52 push compresses consecutive new chunks together in frames of up to 255 KiB,
keeping a frame only where it beats its chunks compressed alone; every chunk stays addressable and hash-checked on its
own.

Applying the switch on the node (README, deploy/csi): a plain pull through the node cache takes 73.8 s; delta apply
(#41) takes 5.7 s without and 8.9 s with the node cache (4.8 s, 3.6 MB read, with frames), and a CSI pod is Running
12 s after creation.

## GPU: TensorRT-LLM engines (#56)

The same problem exists on GPUs when engines are compiled ahead of time. On the lab A100 (80 GB PCIe, TensorRT-LLM
1.0.0, Llama-3.1-8B-Instruct bf16, TP 1), every `trtllm-build` engine embeds the weights: each is 16.14 GB, and 99.8 %
of its bytes are in the checkpoint.

| variant (max batch, max seq / input) | build | engine |
|---|---|---|
| b2-s2k (2, 2048 / 1024) | 61 s | 16,135,764,052 B |
| b8-s2k (8, 2048 / 1024) | 65 s | 16,135,782,004 B |
| b2-s4k (2, 4096 / 2048) | 103 s | 16,135,764,228 B |
| b8-s4k (8, 4096 / 2048) | 99 s | 16,135,782,004 B |

| storing the four engines | size |
|---|---|
| raw | 64.5 GB |
| fixed 1 MiB blocks (dedup) | 48.4 GB (with a rebuild: 1.67×) |
| FastCDC dedup | 16.15 GB (4.00×) |
| **onggi** | **10.85 GB** (5.95×); each engine after the first adds 5.8 MB new, 1.2–1.3 MB packed |

A rebuild of the same configuration is not byte-identical (3,021 bytes differ in the first 8 MB), which also costs
5.8 MB. An engine restored by `onggi pull` is byte-identical and gives the same greedy tokens as the built one (32 of
32).

`onggi diff`: between engines of the same max batch (b8-s2k → b8-s4k), 99.96 % sits at the same offset (33 changed
runs, all in the first 8 MB), and delta apply reads 1.2 MB and leaves 16.1 GB in place. A different max batch makes the
plan in front of the weights 17,952 B longer, which shifts every weight by an amount that is not a multiple of 4 KiB:
0 % stays at the same offset (99.96 % is elsewhere in the base), so delta apply does not take the base and a plain pull
reads 10.8 GB. Since #58 a pull reads such bytes from the base's files where they are, without consuming the base: on
the ATOM set, prefill C128 next to a decode B2 copy (0 % at the same offsets, 79 % elsewhere) read 1.93 GB from the
store instead of 9.47 GB and 11.1 GB from the base, byte-identical.

Unlike ATOM, sub-chunks are not used here (TensorRT engines are kind `other`); 64 KiB chunks already find the shifted
weights.
