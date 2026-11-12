# VarMorph layout coding and device rebuild (prototype)

- `classify.py`: classifies each 64 KiB window of a compiled artifact (weights by numeric type, code, padding, text) from byte statistics, without parsing the format.
- `layout_coder.py`: codes a target variant against sibling(s) at any byte offset below chunk size (sub-block shift search, motion prediction with merge candidates, run merging, XOR residual, intra codec with bf16 byte planes, fill). It decodes every target and compares it byte for byte. With `VCODEC_DUMP=1` it writes the descriptor (`*.runs.npz`).
- `device_rebuild.py`: rebuilds a target on the device from a resident sibling. `replay` is out of place. `inplace` uses a ring of saved windows, KEEP for unmoved runs, absorb below a measured threshold, folding, and batched copies on NPU-B.
- `npu_d2d_bench.py`: device copy and upload costs per platform.
- `rbln_vmshim.cpp`: C shim over `rbln_memcpy_v2v_multi` / `rbln_memcpy_h2v_multi` of the NPU-B runtime API.
