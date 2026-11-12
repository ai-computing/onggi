#!/usr/bin/env python3
"""Apply a VarMorph descriptor file on a node: expand it, then restore the target file.

  expand DESC.vmd.npz[.zst] REF OUT.runs.npz
      Read the descriptor (ops, residual, intra, tail) and the sibling REF and write the copy list
      the restorers read: match runs (dst, src, len) and, for every other block, its bytes
      (fill, residual XOR the sibling block it predicts from, new content). Only the sibling
      blocks that residuals predict from are read. Timed: decompress + expand.
  new|inplace DESC REF WORKDIR OUT.json [COLD]
      expand, then restore the target into a new file / over a private copy of the sibling
      (host_restore.py), sync, and compare with the SHA-256 recorded in the descriptor's name
      file (the target itself, read after the clock). COLD=1 evicts the sibling from the page
      cache first (posix_fadvise DONTNEED).
The copy of the sibling for `inplace` is made before the clock.
"""
import io
import json
import os
import subprocess
import sys
import time

import numpy as np
from numba import njit

B = 64
OP_MATCH, OP_FILL, OP_RES, OP_INTRA = 0, 1, 2, 3


@njit(cache=True)
def count(ops):
    """Number of match runs, and of non-match blocks."""
    op, nm, nb_other = 0, 0, 0
    while op < len(ops):
        m = ops[op]
        op += 1
        L, s = 0, 0
        while True:
            b = np.int64(ops[op])
            op += 1
            L |= (b & 0x7F) << s
            s += 7
            if b < 0x80:
                break
        if m == OP_MATCH or m == OP_RES:
            while True:
                b = np.int64(ops[op])
                op += 1
                if b < 0x80:
                    break
            if m == OP_MATCH:
                nm += 1
            else:
                nb_other += L
        elif m == OP_FILL:
            op += 8
            nb_other += L
        else:
            nb_other += L
    return nm, nb_other


@njit(cache=True)
def expand(R, ops, res, intra, nm, nb_other):
    dst = np.empty(nm, np.int64)
    src = np.empty(nm, np.int64)
    ln = np.empty(nm, np.int64)
    other = np.empty(nb_other, np.int64)
    data = np.empty(nb_other * B, np.uint8)
    op, rp, ip, prev_d, i, km, ko = 0, 0, 0, 0, 0, 0, 0
    while op < len(ops):
        m = ops[op]
        op += 1
        L, s = 0, 0
        while True:
            b = np.int64(ops[op])
            op += 1
            L |= (b & 0x7F) << s
            s += 7
            if b < 0x80:
                break
        a = i * B
        if m == OP_MATCH or m == OP_RES:
            z, s = 0, 0
            while True:
                b = np.int64(ops[op])
                op += 1
                z |= (b & 0x7F) << s
                s += 7
                if b < 0x80:
                    break
            d = prev_d + ((z >> 1) ^ -(z & 1))
            prev_d = d
            if m == OP_MATCH:
                dst[km] = a
                src[km] = a + d
                ln[km] = L * B
                km += 1
            else:
                for k in range(L * B):
                    data[ko * B + k] = R[a + d + k] ^ res[rp + k]
                rp += L * B
                for k in range(L):
                    other[ko + k] = i + k
                ko += L
        elif m == OP_FILL:
            for k in range(L * B):
                data[ko * B + k] = ops[op + (k & 7)]
            op += 8
            for k in range(L):
                other[ko + k] = i + k
            ko += L
        else:
            data[ko * B:(ko + L) * B] = intra[ip:ip + L * B]
            ip += L * B
            for k in range(L):
                other[ko + k] = i + k
            ko += L
        i += L
    return dst, src, ln, other, data


def read_desc(path):
    if path.endswith(".zst"):
        raw = subprocess.run(["zstd", "-q", "-d", "-c", "--long=27", path], capture_output=True, check=True).stdout
        return np.load(io.BytesIO(raw))
    return np.load(path)


def do_expand(desc, ref, out):
    t0 = time.perf_counter()
    d = read_desc(desc)
    ops, res, intra, tail = d["ops"], d["res"], d["intra"], d["tail"]
    t1 = time.perf_counter()
    R = np.memmap(ref, dtype=np.uint8, mode="r")
    nm, no = count(ops)
    dst, src, ln, other, data = expand(R, ops, res, intra, nm, no)
    t2 = time.perf_counter()
    np.savez(out, dst=dst, src=src, len=ln, other=other, other_mode=np.zeros(len(other), np.uint8),
             patch_data=data, tail=tail, v_bytes=d["v_bytes"], refs=np.array([ref]), target=d["target"])
    return dict(desc_bytes=os.path.getsize(desc), decompress_s=t1 - t0, expand_s=t2 - t1, runs=int(nm),
                other_blocks=int(no), patch_bytes=int(no * B))


if __name__ == "__main__":
    mode = sys.argv[1]
    here = os.path.dirname(os.path.abspath(__file__))
    if mode == "expand":
        expand(np.zeros(64, np.uint8), np.zeros(0, np.uint8), np.zeros(0, np.uint8), np.zeros(0, np.uint8), 0, 0)
        print(json.dumps(do_expand(sys.argv[2], sys.argv[3], sys.argv[4])))
    else:
        desc, ref, work, out = sys.argv[2:6]
        cold = len(sys.argv) > 6 and sys.argv[6] == "1"
        os.makedirs(work, exist_ok=True)
        expand(np.zeros(64, np.uint8), np.zeros(0, np.uint8), np.zeros(0, np.uint8), np.zeros(0, np.uint8), 0, 0)
        count(np.zeros(0, np.uint8))
        runs = os.path.join(work, "apply.runs.npz")
        if cold:
            fd = os.open(ref, os.O_RDONLY)
            os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
            os.close(fd)
        t0 = time.perf_counter()
        e = do_expand(desc, ref, runs)
        t1 = time.perf_counter()
        p = subprocess.run([sys.executable, os.path.join(here, "host_restore.py"), mode, runs, work, out + ".restore"],
                           capture_output=True, text=True, check=True)
        t2 = time.perf_counter()
        r = json.load(open(out + ".restore"))
        res = dict(mode=mode, cold=cold, expand=e, expand_total_s=t1 - t0, restore_s=r["restore_s"],
                   process_s=t2 - t1, apply_s=(t1 - t0) + r["restore_s"], correct=r["correct"],
                   bytes_written=r["bytes_written"], load=os.getloadavg())
        json.dump(res, open(out, "w"), indent=1)
        print(json.dumps(res))
