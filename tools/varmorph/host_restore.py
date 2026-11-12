#!/usr/bin/env python3
"""Restore a target file on the host from a sibling file and a copy list (runs.npz), timed.

Modes:
  new      write the target into a new file: copy every run from the sibling, then the patch
           blocks (blocks no run covers), then fsync.  Same output contract as `xdelta3 -d`.
  inplace  turn a private copy of the sibling into the target in place, window by window in
           target order, saving each window's original bytes in a ring first (as the device
           rebuild does); runs whose move is zero are not touched; then the patch; then fsync.
The patch bytes are taken from the target file at the blocks no run covers (they stand for the
residual and new content that the descriptor carries). The result is compared with the target's
SHA-256. The copy of the sibling for `inplace` is made before the clock and is not timed.
argv: MODE RUNS.npz WORKDIR OUT.json
"""
import hashlib
import json
import os
import shutil
import sys
import time

import numpy as np

BLK = 64
WIN = 64 << 20
RING = 256 << 20


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            b = f.read(64 << 20)
            if not b:
                break
            h.update(b)
    return h.hexdigest()


def main():
    mode, runs_path, work, out = sys.argv[1:5]
    r = np.load(runs_path)
    dst, src, ln = r["dst"].astype(np.int64), r["src"].astype(np.int64), r["len"].astype(np.int64)
    ref, tgt = str(r["refs"][0]), str(r["target"])
    vb = int(r["v_bytes"])
    al = vb // BLK * BLK
    T = np.memmap(tgt, dtype=np.uint8, mode="r")
    need = np.ones(al // BLK, dtype=bool)
    for d, l in zip(dst.tolist(), ln.tolist()):
        need[d // BLK:(d + l) // BLK] = False
    other = r["other"]
    need[other[other < al // BLK]] = True
    patch_blocks = np.nonzero(need)[0]
    brk = np.nonzero(np.diff(patch_blocks) != 1)[0] + 1
    pr = list(zip(np.concatenate([[0], brk]).tolist(), np.concatenate([brk, [len(patch_blocks)]]).tolist()))
    if "patch_data" in r.files:
        # bytes carried by the descriptor (vm_apply.py expand): the target file is not read
        pd, oth = r["patch_data"], r["other"]
        pos = {int(b): i for i, b in enumerate(oth.tolist())}
        assert all(int(b) in pos for b in patch_blocks.tolist()), "descriptor does not cover every block"
        patch = []
        for a, b in pr:
            i0 = pos[int(patch_blocks[a])]
            patch.append((int(patch_blocks[a]) * BLK, pd[i0 * BLK:(i0 + (b - a)) * BLK]))
        tail = r["tail"]
    else:
        patch = [(int(patch_blocks[a]) * BLK, np.array(T[int(patch_blocks[a]) * BLK:int(patch_blocks[b - 1]) * BLK + BLK]))
                 for a, b in pr]
        tail = np.array(T[al:vb])
    os.makedirs(work, exist_ok=True)
    path = os.path.join(work, os.path.basename(tgt) + "." + mode)
    res = dict(mode=mode, runs=runs_path, ref=ref, target=tgt, v_bytes=vb, n_runs=int(len(dst)),
               patch_bytes=int(len(patch_blocks) * BLK), patch_ranges=len(pr),
               load=os.getloadavg())
    if mode == "inplace":
        shutil.copyfile(ref, path)
        os.truncate(path, max(vb, os.path.getsize(ref)))
        with open(path, "rb") as f:
            os.fsync(f.fileno())
    order = np.argsort(dst, kind="stable")
    dst, src, ln = dst[order], src[order], ln[order]
    t0 = time.perf_counter()
    written = 0
    if mode == "new":
        R = np.memmap(ref, dtype=np.uint8, mode="r")
        O = np.memmap(path, dtype=np.uint8, mode="w+", shape=(vb,))
        for d, s_, l in zip(dst.tolist(), src.tolist(), ln.tolist()):
            O[d:d + l] = R[s_:s_ + l]
        for off, b in patch:
            O[off:off + len(b)] = b
        O[al:vb] = tail
        written = vb
    else:
        O = np.memmap(path, dtype=np.uint8, mode="r+")
        nslots = RING // WIN + 1
        ring = [None] * nslots
        # far sources (before the ring) are saved before the first write
        pieces = []
        for d, s_, l in zip(dst.tolist(), src.tolist(), ln.tolist()):
            while l > 0:
                take = min(l, WIN - d % WIN)
                pieces.append((d, s_, take))
                d, s_, l = d + take, s_ + take, l - take
        far = {}
        for d, s_, l in pieces:
            w = d // WIN
            lo = max(w * WIN - (nslots - 1) * WIN, 0)
            if s_ < lo and d != s_:
                far[(s_, l)] = np.array(O[s_:s_ + l])
        by_w = {}
        for p in pieces:
            by_w.setdefault(p[0] // WIN, []).append(p)
        last = (al - 1) // WIN
        for w in range(last + 1):
            a = w * WIN
            lst = [p for p in by_w.get(w, []) if p[0] != p[1]]
            ring[w % nslots] = (a, np.array(O[a:a + WIN])) if lst or True else None
            for d, s_, l in lst:
                if s_ >= a + WIN:
                    O[d:d + l] = O[s_:s_ + l]
                elif (s_, l) in far:
                    O[d:d + l] = far[(s_, l)]
                else:
                    x, dd, rem = s_, d, l
                    while rem > 0:
                        if x >= a + WIN:                  # not yet overwritten
                            O[dd:dd + rem] = O[x:x + rem]
                            break
                        sw = x // WIN
                        base, buf = ring[sw % nslots]
                        take = min(rem, base + WIN - x)
                        O[dd:dd + take] = buf[x - base:x - base + take]
                        x, dd, rem = x + take, dd + take, rem - take
                written += l
        for off, b in patch:
            O[off:off + len(b)] = b
            written += len(b)
        O[al:vb] = tail
    O.flush()
    del O
    with open(path, "rb+") as f:
        if mode == "inplace":
            f.truncate(vb)
        os.fsync(f.fileno())
    t1 = time.perf_counter()
    res.update(restore_s=t1 - t0, bytes_written=int(written))
    res["sha256"] = sha(path)
    res["target_sha256"] = sha(tgt)
    res["correct"] = res["sha256"] == res["target_sha256"]
    os.remove(path)
    with open(out, "w") as f:
        json.dump(res, f, indent=1)
    print(json.dumps({k: v for k, v in res.items() if k != "load"}))


if __name__ == "__main__":
    main()
