#!/usr/bin/env python3
"""Cohort 2026-11-12 (10): a video-coding-style lossless coder for a compiled variant against siblings.

A target variant v is coded against reference variants (the siblings a node may hold), the way a
P-frame is coded against reference frames. Tools taken from video coding, and why:

Matching
- block matching: v is split into 64-byte blocks, and each block is searched in the references by
  exact hash, verified byte by byte. Exact matching replaces a SAD search, since the result must be
  lossless.
- sub-block motion (the "sub-pel" step): content moves by offsets that are not multiples of 64
  bytes (a first, aligned-only run matched 211 KB of a prefill pair that content-defined chunks
  share in GB). A rolling hash over every byte position of the references finds a sample of target
  blocks. The phases (offset mod 64) of those shifts are then indexed, so the main search stays one
  lookup per block.
- MV prediction: the block at the predicted position (the previous match plus the distance since)
  is compared first. A hit is a "skip": no lookup and no new vector.
- variable blocks: consecutive blocks whose references are consecutive merge into one run.

Coding
- MVD + entropy: each run is coded as its length and the difference of its motion vector from the
  previous one (zigzag varints), then entropy-coded (zstd).
- inter residual: a block that matches nothing exactly but differs from its predicted block in at
  most T bytes is coded as the XOR with that block.
- intra: any other block is raw content. Its bytes are coded with zstd, alone and after splitting
  2-byte words into byte planes (bf16).
- fill (DC): a block of one repeated 8-byte word is coded as that word.
- multi-reference: several references share one index; a vector names its reference through the
  global offset.

Not taken: transform and quantization (lossy) and loop filters.

Every coded target is decoded and compared with v byte for byte before its size is reported. The
decode is timed: a single-threaded rebuild into host memory from references already in memory.
argv: OUT.json T REF[,REF...] TARGET [REF[,REF...] TARGET ...]
"""
import json
import os
import shutil
import subprocess
import sys
import time

import numpy as np

try:
    from numba import njit
except ImportError:  # slow fallback for small tests
    def njit(*a, **k):
        if a and callable(a[0]):
            return a[0]
        return lambda f: f

B = 64
W = B // 8
K = (np.random.default_rng(12).integers(1, 2**63, size=W, dtype=np.uint64) | np.uint64(1))
OP_MATCH, OP_FILL, OP_RES, OP_INTRA = 0, 1, 2, 3
BASE = np.uint64(1099511628211)
SAMPLES = 1 << 16
NMERGE = int(os.environ.get("VCODEC_NMERGE", "4"))  # merge candidates (0: off)
NOMV = -(1 << 62)
BITS = 26


def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)


def block_hashes(buf):
    n = len(buf) // B
    h = np.empty(n, dtype=np.uint64)
    step = (1 << 28) // B
    for i in range(0, n, step):
        j = min(n, i + step)
        w = np.frombuffer(buf[i * B:j * B].tobytes(), dtype=np.uint64).reshape(-1, W) if buf.ctypes.data % 8 else \
            buf[i * B:j * B].view(np.uint64).reshape(-1, W)
        h[i:j] = ((w ^ (w >> np.uint64(29))) * K).sum(axis=1, dtype=np.uint64)
    return h


@njit(cache=True)
def is_fill(V, o):
    for k in range(8, B):
        if V[o + k] != V[o + (k & 7)]:
            return False
    return True


@njit(cache=True)
def rk(V, o):
    h = np.uint64(0)
    for k in range(B):
        h = h * BASE + np.uint64(V[o + k])
    return h


@njit(cache=True)
def find_shifts(R, V, sample_off, cap):
    """Rolling hash over every byte of R; returns (ref_off, tgt_off) of verified sample hits."""
    m = len(sample_off)
    sh = np.empty(m, dtype=np.uint64)
    for s in range(m):
        sh[s] = rk(V, sample_off[s])
    order = np.argsort(sh)
    ss = sh[order]
    so = sample_off[order]
    bitmap = np.zeros(1 << (BITS - 3), dtype=np.uint8)
    MIX = np.uint64(0x9E3779B97F4A7C15)
    for s in range(m):
        b = (ss[s] * MIX) >> np.uint64(64 - BITS)
        bitmap[b >> np.uint64(3)] |= np.uint8(1) << np.uint8(b & np.uint64(7))
    pw = np.uint64(1)
    for k in range(B):
        pw = pw * BASE
    out_r = np.empty(cap, dtype=np.int64)
    out_t = np.empty(cap, dtype=np.int64)
    nout = 0
    n = len(R)
    if n < B:
        return out_r[:0], out_t[:0]
    h = rk(R, 0)
    pos = 0
    while True:
        b = (h * MIX) >> np.uint64(64 - BITS)
        if bitmap[b >> np.uint64(3)] & (np.uint8(1) << np.uint8(b & np.uint64(7))):
            lo, hi = 0, m
            while lo < hi:
                mid = (lo + hi) >> 1
                if ss[mid] < h:
                    lo = mid + 1
                else:
                    hi = mid
            c = lo
            while c < m and ss[c] == h and nout < cap:
                t = so[c]
                eq = True
                for k in range(B):
                    if R[pos + k] != V[t + k]:
                        eq = False
                        break
                if eq:
                    out_r[nout] = pos
                    out_t[nout] = t
                    nout += 1
                c += 1
        if pos + B >= n:
            break
        h = h * BASE + np.uint64(R[pos + B]) - np.uint64(R[pos]) * pw
        pos += 1
    return out_r[:nout], out_t[:nout]


@njit(cache=True)
def scan(R, V, sr, spos, hv, T):
    """Choose a mode per target block. ref holds a byte offset in R (or -1)."""
    n = len(hv)
    nr = len(sr)
    nR = len(R)
    mode = np.empty(n, dtype=np.uint8)
    ref = np.full(n, -1, dtype=np.int64)
    last_i = -1
    last_r = -1
    lookups = 0
    skips = 0
    merges = 0
    mc = np.full(NMERGE, NOMV, dtype=np.int64)
    for i in range(n):
        o = i * B
        if is_fill(V, o):
            mode[i] = OP_FILL
            continue
        p = -1
        if last_r >= 0:
            p = last_r + (i - last_i) * B
            if p + B > nR:
                p = -1
        nz = B + 1
        if p >= 0:
            nz = 0
            for k in range(B):
                if R[p + k] != V[o + k]:
                    nz += 1
            if nz == 0:
                mode[i] = OP_MATCH
                ref[i] = p
                last_i = i
                last_r = p
                skips += 1
                continue
        # merge candidates (as in HEVC merge mode): the motions of the last few runs, tried
        # before any hash lookup so that a block found elsewhere does not break the motion field
        mfound = -1
        for c_ in range(NMERGE):
            if mc[c_] == NOMV:
                continue
            q = o + mc[c_]
            if q < 0 or q + B > nR or q == p:
                continue
            eq = True
            for k in range(B):
                if R[q + k] != V[o + k]:
                    eq = False
                    break
            if eq:
                mfound = q
                break
        if mfound >= 0:
            mode[i] = OP_MATCH
            ref[i] = mfound
            last_i = i
            last_r = mfound
            merges += 1
            continue
        lookups += 1
        lo, hi = 0, nr
        x = hv[i]
        while lo < hi:
            mid = (lo + hi) >> 1
            if sr[mid] < x:
                lo = mid + 1
            else:
                hi = mid
        found = -1
        c = lo
        while c < nr and c < lo + 8 and sr[c] == x:
            q = spos[c]
            eq = True
            for k in range(B):
                if R[q + k] != V[o + k]:
                    eq = False
                    break
            if eq:
                found = q
                break
            c += 1
        if found >= 0:
            mode[i] = OP_MATCH
            ref[i] = found
            last_i = i
            last_r = found
            d_ = found - o
            if NMERGE > 0 and mc[0] != d_:
                for c_ in range(NMERGE - 1, 0, -1):
                    mc[c_] = mc[c_ - 1]
                mc[0] = d_
            continue
        if p >= 0 and nz <= T:
            mode[i] = OP_RES
            ref[i] = p
            last_i = i
            last_r = p
            continue
        mode[i] = OP_INTRA
    return mode, ref, lookups, skips, merges


@njit(cache=True)
def put_varint(buf, pos, x):
    while x >= 0x80:
        buf[pos] = (x & 0x7F) | 0x80
        x >>= 7
        pos += 1
    buf[pos] = x
    return pos + 1


@njit(cache=True)
def build(R, V, mode, ref):
    """Merge blocks into runs; write the op stream, the residual stream and the intra stream."""
    n = len(mode)
    ops = np.empty(n * 12 + 64, dtype=np.uint8)
    res = np.empty(np.sum(mode == OP_RES) * B + 1, dtype=np.uint8)
    intra = np.empty(np.sum(mode == OP_INTRA) * B + 1, dtype=np.uint8)
    op = 0
    rp = 0
    ip = 0
    prev_d = 0
    runs = np.zeros(4, dtype=np.int64)
    i = 0
    while i < n:
        m = mode[i]
        j = i + 1
        if m == OP_MATCH or m == OP_RES:
            while j < n and mode[j] == m and ref[j] == ref[j - 1] + B:
                j += 1
        elif m == OP_FILL:
            while j < n and mode[j] == OP_FILL:
                eq = True
                for k in range(8):
                    if V[j * B + k] != V[i * B + k]:
                        eq = False
                        break
                if not eq:
                    break
                j += 1
        else:
            while j < n and mode[j] == OP_INTRA:
                j += 1
        L = j - i
        runs[m] += 1
        ops[op] = m
        op += 1
        op = put_varint(ops, op, L)
        if m == OP_MATCH or m == OP_RES:
            d = ref[i] - i * B
            dd = d - prev_d
            op = put_varint(ops, op, (dd << 1) ^ (dd >> 63))
            prev_d = d
            if m == OP_RES:
                for b in range(i, j):
                    for k in range(B):
                        res[rp] = R[ref[b] + k] ^ V[b * B + k]
                        rp += 1
        elif m == OP_FILL:
            for k in range(8):
                ops[op] = V[i * B + k]
                op += 1
        else:
            intra[ip:ip + L * B] = V[i * B:j * B]
            ip += L * B
        i = j
    return ops[:op], res[:rp], intra[:ip], runs


@njit(cache=True)
def decode(R, ops, res, intra, n, tail):
    out = np.empty(n * B + len(tail), dtype=np.uint8)
    op = 0
    rp = 0
    ip = 0
    prev_d = 0
    i = 0
    while op < len(ops):
        m = ops[op]
        op += 1
        L = 0
        s = 0
        while True:
            b = np.int64(ops[op])
            op += 1
            L |= (b & 0x7F) << s
            s += 7
            if b < 0x80:
                break
        a = i * B
        e = a + L * B
        if m == OP_MATCH or m == OP_RES:
            z = 0
            s = 0
            while True:
                b = np.int64(ops[op])
                op += 1
                z |= (b & 0x7F) << s
                s += 7
                if b < 0x80:
                    break
            d = prev_d + ((z >> 1) ^ -(z & 1))
            prev_d = d
            out[a:e] = R[a + d:e + d]
            if m == OP_RES:
                for k in range(L * B):
                    out[a + k] ^= res[rp + k]
                rp += L * B
        elif m == OP_FILL:
            for k in range(a, e):
                out[k] = ops[op + ((k - a) & 7)]
            op += 8
        else:
            out[a:e] = intra[ip:ip + L * B]
            ip += L * B
        i += L
    out[n * B:] = tail
    return out


def zsize(arr, level=19):
    if len(arr) == 0:
        return 0
    if shutil.which("zstd"):
        lv = ["--ultra", "-22"] if level > 19 else ["-%d" % level]
        p = subprocess.run(["zstd", "-q", "-c", "-T16", "--long=27"] + lv, input=arr.tobytes(), capture_output=True, check=True)
        return len(p.stdout)
    try:
        import zstandard
        return len(zstandard.ZstdCompressor(level=min(level, 19), threads=16).compress(arr.tobytes()))
    except ImportError:
        import lzma
        return len(lzma.compress(arr.tobytes(), preset=6))


def intra_size(arr):
    """Best of zstd on the stream and zstd on its two byte planes (bf16 split)."""
    plain = zsize(arr, 9)
    if len(arr) < 2:
        return plain, "zstd"
    a = arr[: len(arr) // 2 * 2]
    planes = zsize(np.ascontiguousarray(a[0::2]), 9) + zsize(np.ascontiguousarray(a[1::2]), 9) + (len(arr) - len(a))
    return (planes, "planes") if planes < plain else (plain, "zstd")


def load(paths):
    sizes = [os.path.getsize(p) for p in paths]
    pads = [(-s) % B for s in sizes]
    R = np.empty(sum(sizes) + sum(pads), dtype=np.uint8)
    o = 0
    for p, s, pd in zip(paths, sizes, pads):
        with open(p, "rb") as f:
            f.readinto(memoryview(R)[o:o + s])
        R[o + s:o + s + pd] = 0
        o += s + pd
    return R


def run_stats(mode, ref, nontrivial_bytes):
    idx = np.nonzero(mode == OP_MATCH)[0]
    if len(idx) == 0:
        return {}
    r = ref[idx]
    brk = np.ones(len(idx), dtype=bool)
    brk[1:] = (idx[1:] != idx[:-1] + 1) | (r[1:] != r[:-1] + B)
    st = np.nonzero(brk)[0]
    run_bytes = np.diff(np.append(st, len(idx))) * B
    d = r[st] - idx[st] * B
    cov = {str(L): float(run_bytes[run_bytes >= L].sum() / nontrivial_bytes)
           for L in [64, 256, 1024, 4096, 16384, 65536, 1 << 20]}
    ud, inv = np.unique(d, return_inverse=True)
    w = np.bincount(inv, weights=run_bytes)
    o = np.argsort(-w)
    cw = np.cumsum(w[o]) / run_bytes.sum()
    jd = np.diff(d)
    out = {"match_runs": int(len(st)), "median_run_bytes": float(np.median(run_bytes)),
           "coverage_by_run_ge": cov, "distinct_deltas": int(len(ud)),
           "deltas_for_90pct": int(np.searchsorted(cw, 0.9) + 1),
           "top_deltas": [{"delta": int(ud[k]), "share": float(w[k] / run_bytes.sum())} for k in o[:10]]}
    if len(jd):
        uj, invj = np.unique(jd, return_inverse=True)
        wj = np.bincount(invj, weights=run_bytes[1:])
        oj = np.argsort(-wj)
        cj = np.cumsum(wj[oj]) / max(run_bytes[1:].sum(), 1)
        out.update({"distinct_jumps": int(len(uj)), "jumps_for_90pct": int(np.searchsorted(cj, 0.9) + 1),
                    "top_jumps": [{"jump": int(uj[k]), "share": float(wj[k] / run_bytes[1:].sum())} for k in oj[:10]]})
    return out


def code_one(refs, target, T, modes_path=None):
    t0 = time.time()
    R = load(refs)
    V = np.fromfile(target, dtype=np.uint8)
    nb = len(V) // B
    tail = V[nb * B:].copy()
    Vb = V[: nb * B]
    # sub-block motion: phases of the shifts found for a sample of target blocks
    cand = np.linspace(0, nb - 1, SAMPLES * 2).astype(np.int64)
    cand = np.array([c * B for c in cand if not is_fill(Vb, c * B)][:SAMPLES], dtype=np.int64)
    hr_, ht_ = find_shifts(R, Vb, cand, 1 << 22)
    shift = hr_ - ht_
    ph = np.bincount(((shift % B) + B) % B, minlength=B)
    hit_blocks = len(np.unique(ht_))
    order_ph = np.argsort(-ph)
    phases = [0]
    acc = ph[0]
    for p in order_ph:
        if ph[p] == 0 or len(phases) >= 8 or acc >= 0.99 * max(ph.sum(), 1):
            break
        if p != 0:
            phases.append(int(p))
            acc += ph[p]
    us, cs = np.unique(shift, return_counts=True)
    oc = np.argsort(-cs)[:10]
    t1 = time.time()
    log("phases", phases, "sample hits", int(hit_blocks), "of", len(cand))
    hs, ps = [], []
    for p in phases:
        n = (len(R) - p) // B
        hs.append(block_hashes(R[p:p + n * B]))
        ps.append(p + np.arange(n, dtype=np.int64) * B)
    hr = np.concatenate(hs)
    pr = np.concatenate(ps)
    del hs, ps
    order = np.argsort(hr, kind="stable")
    sr = hr[order]
    spos = pr[order]
    del hr, pr, order
    hv = block_hashes(Vb)
    t2 = time.time()
    mode, ref, lookups, skips, merges = scan(R, Vb, sr, spos, hv, T)
    ops, res, intra, runs = build(R, Vb, mode, ref)
    t3 = time.time()
    decode(R[:1 << 20], ops[:0], res, intra, 0, tail[:0])  # compile outside the timed call
    t4 = time.time()
    out = decode(R, ops, res, intra, nb, tail)
    t5 = time.time()
    ok = bool(np.array_equal(out, V))
    del out
    log("decoded", ok, round(t5 - t4, 2), "s")
    cnt = np.bincount(mode, minlength=4) * B
    if modes_path and os.environ.get("VCODEC_DUMP"):
        # the descriptor itself, for a device-side replay: match runs (target byte offset,
        # reference byte offset, length) and the other blocks (target block index, mode)
        idx = np.nonzero(mode == OP_MATCH)[0]
        rr = ref[idx]
        brk = np.ones(len(idx), dtype=bool)
        brk[1:] = (idx[1:] != idx[:-1] + 1) | (rr[1:] != rr[:-1] + B)
        st = np.nonzero(brk)[0]
        ln = np.diff(np.append(st, len(idx))) * B
        other = np.nonzero(mode != OP_MATCH)[0]
        np.savez(modes_path.replace(".modes.npy", ".runs.npz"), dst=idx[st] * B, src=rr[st], len=ln,
                 other=other, other_mode=mode[other], v_bytes=len(V), refs=np.array(refs), target=np.array(target))
    if modes_path:  # per 64 KiB window: blocks coded as match / fill / residual / intra
        nw = nb // 1024
        wm = np.bincount((np.arange(nw * 1024) // 1024) * 4 + mode[:nw * 1024], minlength=nw * 4)
        np.save(modes_path, wm.reshape(nw, 4).astype(np.uint32))
    s_ops = zsize(ops, 19)
    s_res = zsize(res, 19)
    s_intra, intra_how = intra_size(intra)
    total = s_ops + s_res + s_intra + len(tail)
    nontriv = int(len(V) - cnt[1])
    return {
        "refs": refs, "target": target, "T": T, "v_bytes": int(len(V)), "lossless": ok,
        "sample": {"blocks": int(len(cand)), "hit_blocks": int(hit_blocks), "hits": int(len(shift)),
                   "phase_hist_top": {int(p): int(ph[p]) for p in order_ph[:8]}, "phases_indexed": phases,
                   "top_shifts": [{"shift": int(us[k]), "count": int(cs[k])} for k in oc]},
        "bytes_by_mode": {"match": int(cnt[0]), "fill": int(cnt[1]), "residual": int(cnt[2]), "intra": int(cnt[3])},
        "runs_by_mode": {"match": int(runs[0]), "fill": int(runs[1]), "residual": int(runs[2]), "intra": int(runs[3])},
        "lookups": int(lookups), "skips": int(skips), "merges": int(merges), "nmerge": NMERGE,
        "match_structure": run_stats(mode, ref, max(nontriv, 1)),
        "coded": {"ops_raw": int(len(ops)), "ops": s_ops, "residual_raw": int(len(res)), "residual": s_res,
                  "intra_raw": int(len(intra)), "intra": s_intra, "intra_codec": intra_how, "total": total},
        "seconds": {"shift_search": round(t1 - t0, 1), "index": round(t2 - t1, 1), "encode": round(t3 - t2, 1),
                    "decode_host": round(t5 - t4, 2)},
        "decode_GBps": round(len(V) / max(t5 - t4, 1e-9) / 1e9, 2),
    }


if __name__ == "__main__":
    out, T = sys.argv[1], int(sys.argv[2])
    a = sys.argv[3:]
    res = []
    for k in range(0, len(a), 2):
        refs = a[k].split(",")
        log("code", refs, "->", a[k + 1])
        res.append(code_one(refs, a[k + 1], T, "%s.%d.modes.npy" % (out, k // 2)))
        with open(out, "w") as f:
            json.dump(res, f, indent=1)
        r = res[-1]
        log("result", json.dumps(r["coded"]), json.dumps(r["bytes_by_mode"]), json.dumps(r["sample"]["phases_indexed"]),
            "decode", r["seconds"]["decode_host"])
    log("ALL-DONE")
