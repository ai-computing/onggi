#!/usr/bin/env python3
"""Backward reach of the moves of a copy list and the scratch an in-place rebuild needs.
argv: OUT.json RUNS.npz [RUNS.npz ...]   (window 64 MiB, ring 256 MiB as in npu_replay.py)"""
import json, sys
import numpy as np
WIN, RING = 64 << 20, 256 << 20
res = []
for p in sys.argv[2:]:
    r = np.load(p)
    d, s, l = r["dst"].astype(np.int64), r["src"].astype(np.int64), r["len"].astype(np.int64)
    mv = s - d
    moved = mv != 0
    a = (d // WIN) * WIN                      # start of the window being written
    back = np.where(moved, a - s, 0)          # > 0: the source lies before the window (already overwritten)
    ring_lo = np.maximum(a - RING, 0)
    far = moved & (s < ring_lo)
    far_bytes = int(np.minimum(l[far], ring_lo[far] - s[far]).sum())
    res.append(dict(runs=p, target=str(r["target"]), n_runs=int(len(d)), keep_bytes=int(l[~moved].sum()),
                    moved_bytes=int(l[moved].sum()), max_back_bytes=int(back.max()) if len(back) else 0,
                    back_p99_bytes=int(np.percentile(back[moved], 99)) if moved.any() else 0,
                    far_runs=int(far.sum()), far_bytes=far_bytes, scratch_bytes=RING + WIN + far_bytes,
                    v_bytes=int(r["v_bytes"]), ref_bytes=None))
json.dump(res, open(sys.argv[1], "w"), indent=1)
for x in res: print(json.dumps(x))
