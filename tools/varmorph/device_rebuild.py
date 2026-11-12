#!/usr/bin/env python3
"""Rebuild a target variant on the NPU from a resident sibling, with the coder's descriptor.

The reference (sibling) bytes are uploaded once and left on the device: they stand for weights
that are already resident. The timed part rebuilds the target on the device:
  1. folded copies: match runs of equal length whose target and reference offsets advance by
     constant strides are one strided copy (as_strided). The rest are single-run copies.
  2. patch: the 64-byte blocks that match nothing (residual, new content, padding) are uploaded
     as one contiguous buffer and put in place with one index_copy_.
The baseline is an upload of the whole target from host memory, in the same 1 GiB pieces.
The result is read back and its SHA-256 compared with the target file.

Device memory is a list of 1 GiB tensors (int16); a copy never crosses a piece, and copies are
cut at MAXCOPY bytes. Offsets must be even (int16 elements); odd runs are reported and fall
back to the patch.
argv: analyze RUNS.npz OUT.json                      (folding statistics only, no device)
      inplace RUNS.npz OUT.json PLATFORM             (in-place rebuild in the sibling's memory)
      replay  RUNS.npz OUT.json PLATFORM(rngd|rbln)  (device replay)
"""
import hashlib
import json
import os
import sys
import time

import numpy as np

PIECE = 1 << 30           # bytes per device tensor
MAXCOPY = 256 << 20       # bytes per copy call
BLK = 64


def split_runs(dst, src, ln):
    """Cut runs at piece boundaries of both sides and at MAXCOPY."""
    out_d, out_s, out_l = [], [], []
    for d, s, l in zip(dst.tolist(), src.tolist(), ln.tolist()):
        while l > 0:
            take = min(l, PIECE - d % PIECE, PIECE - s % PIECE, MAXCOPY)
            out_d.append(d)
            out_s.append(s)
            out_l.append(take)
            d += take
            s += take
            l -= take
    return np.array(out_d, np.int64), np.array(out_s, np.int64), np.array(out_l, np.int64)


GAP = int(os.environ.get("REPLAY_GAP", str(1 << 20)))  # bytes of target a merged copy may cover


# Measured cost of one copy call and of host upload per platform (notes/npu-d2d-20261112):
#   rngd-a3: copy_ per call 120 us, upload 3.7 GB/s; rbln-b3: v2v_multi 1.6 us per copy, upload 21.6 GB/s.
COSTS = {"rngd": dict(call_s=120e-6, h2d_Bps=3.7e9, d2d_Bps=24.4e9),
         "rbln": dict(call_s=1.6e-6, h2d_Bps=21.6e9, d2d_Bps=21.7e9),
         "rbln-unbatched": dict(call_s=5.0e-6, h2d_Bps=21.6e9, d2d_Bps=21.7e9),
         "cpu": dict(call_s=1e-6, h2d_Bps=10e9, d2d_Bps=10e9)}


def estimate(platform, n_copies, copy_bytes, patch_bytes, target_bytes):
    """Predicted seconds of the rebuild and of a full upload, from the measured cost table."""
    c = COSTS.get(platform, COSTS["cpu"])
    rebuild = n_copies * c["call_s"] + copy_bytes / c["d2d_Bps"] + patch_bytes / c["h2d_Bps"]
    upload = target_bytes / c["h2d_Bps"]
    return rebuild, upload


def absorb_threshold(platform):
    """A run shorter than this is cheaper to upload in the patch than to copy with its own call:
    the bytes the host link moves in the time of one call."""
    c = COSTS.get(platform, COSTS["cpu"])
    return int(c["call_s"] * c["h2d_Bps"]) // BLK * BLK


ABSORB = int(os.environ.get("REPLAY_ABSORB", str(64 << 10)))  # bytes: shorter off-motion runs go to the patch
LOOK = int(os.environ.get("REPLAY_LOOK", str(1 << 20)))       # bytes of target to look ahead for the motion


def absorb(dst, src, ln, limit=ABSORB, look=LOOK):
    """Rate-distortion step: a short run whose motion differs from the surrounding segment is not
    copied on its own; the segment's copy runs over it and its blocks join the patch. Returns the
    kept runs and the absorbed byte ranges (target offset, length)."""
    if limit <= 0 or len(dst) == 0:
        return dst, src, ln, []
    o = np.argsort(dst, kind="stable")
    dst, src, ln = dst[o].tolist(), src[o].tolist(), ln[o].tolist()
    mv = [s_ - d for d, s_ in zip(dst, src)]
    keep, gone = [], []
    i, n = 0, len(dst)
    while i < n:
        keep.append(i)
        cur = mv[i]
        j = i + 1
        while j < n:
            if mv[j] == cur:
                keep.append(j)
                j += 1
                continue
            # skip short off-motion runs if the segment's motion resumes within LOOK bytes
            k, skipped = j, 0
            while k < n and mv[k] != cur and ln[k] <= limit and dst[k] - dst[j] <= look:
                skipped += 1
                k += 1
            if k < n and mv[k] == cur and skipped:
                gone.extend(range(j, k))
                j = k
            else:
                break
        i = j
    ka = np.array(keep, dtype=np.int64)
    d, s_, l = np.array(dst)[ka], np.array(src)[ka], np.array(ln)[ka]
    return d, s_, l, [(dst[g], ln[g]) for g in gone]


def coalesce(dst, src, ln, gap=GAP):
    """Merge runs with the same motion (src - dst) whose target gap is <= gap: one copy covers the
    gap, and the patch overwrites the gap blocks afterwards (skip + residual)."""
    if gap <= 0 or len(dst) == 0:
        return dst, src, ln
    o = np.argsort(dst, kind="stable")
    dst, src, ln = dst[o], src[o], ln[o]
    out = [[dst[0], src[0], ln[0]]]
    for d, s_, l in zip(dst[1:].tolist(), src[1:].tolist(), ln[1:].tolist()):
        pd, ps, pl = out[-1]
        if s_ - d == ps - pd and 0 <= d - (pd + pl) <= gap:
            out[-1][2] = d + l - pd
        else:
            out.append([d, s_, l])
    a = np.array(out, dtype=np.int64)
    return a[:, 0], a[:, 1], a[:, 2]


def fold(dst, src, ln):
    """Greedy: consecutive runs (by target offset) with equal length and constant strides, inside
    one piece on each side, form a group (d0, s0, len, count, dd, ds)."""
    order = np.argsort(dst, kind="stable")
    dst, src, ln = dst[order], src[order], ln[order]
    groups = []
    i, n = 0, len(dst)
    while i < n:
        j = i + 1
        if j < n and ln[j] == ln[i] and dst[j] // PIECE == dst[i] // PIECE and src[j] // PIECE == src[i] // PIECE:
            dd, ds = dst[j] - dst[i], src[j] - src[i]
            if dd >= ln[i] and ds > 0:
                while (j + 1 < n and ln[j + 1] == ln[i] and dst[j + 1] - dst[j] == dd and src[j + 1] - src[j] == ds
                       and dst[j + 1] // PIECE == dst[i] // PIECE and src[j + 1] // PIECE == src[i] // PIECE
                       and (j + 1 - i + 1) * ln[i] <= MAXCOPY):
                    j += 1
                j += 1
            else:
                dd = ds = 0
                j = i + 1
        else:
            dd = ds = 0
        groups.append((int(dst[i]), int(src[i]), int(ln[i]), j - i, int(dd), int(ds)))
        i = j
    return groups


def load(runs_path):
    r = np.load(runs_path)
    dst, src, ln = r["dst"].astype(np.int64), r["src"].astype(np.int64), r["len"].astype(np.int64)
    odd = (dst % 2 != 0) | (src % 2 != 0)
    return r, dst[~odd], src[~odd], ln[~odd], dst[odd], ln[odd]


def analyze(runs_path, out):
    r, dst, src, ln, odd_d, odd_l = load(runs_path)
    dst, src, ln, gone = absorb(dst, src, ln)
    dst, src, ln = coalesce(dst, src, ln)
    d2, s2, l2 = split_runs(dst, src, ln)
    g = fold(d2, s2, l2)
    cnt = np.array([x[3] for x in g])
    res = dict(gap=GAP, absorb=ABSORB, absorbed_runs=len(gone), absorbed_bytes=int(sum(x[1] for x in gone)),
               coalesced_runs=int(len(dst)), runs=int(len(r["dst"])), odd_runs=int(len(odd_d)), odd_bytes=int(odd_l.sum()),
               split_runs=int(len(d2)), groups=len(g), single_run_groups=int((cnt == 1).sum()),
               bytes_in_multi_run_groups=int(sum(x[2] * x[3] for x in g if x[3] > 1)),
               match_bytes=int(l2.sum()), other_blocks=int(len(r["other"])), v_bytes=int(r["v_bytes"]))
    with open(out, "w") as f:
        json.dump(res, f, indent=1)
    print(json.dumps(res))
    return r, g, odd_d, odd_l, gone


def replay(runs_path, out, platform):
    r, g, odd_d, odd_l, gone = analyze(runs_path, out + ".fold.json")
    import torch
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    from npu_d2d_bench import device
    dev, sync, pk = device(platform)
    ref_path, tgt_path = str(r["refs"][0]), str(r["target"])
    vb = int(r["v_bytes"])
    res = dict(schema="npu-replay.v1", platform=platform, device=dev, ref=ref_path, target=tgt_path, v_bytes=vb,
               groups=len(g), load=(os.getloadavg() if hasattr(os, "getloadavg") else None))

    def pieces_of(nbytes):
        return [min(PIECE, nbytes - o) for o in range(0, nbytes, PIECE)]

    def host_bytes(path, nbytes):
        a = np.zeros(nbytes + (nbytes % 2), dtype=np.uint8)
        with open(path, "rb") as f:
            f.readinto(memoryview(a)[:os.path.getsize(path)])
        return a

    ref = host_bytes(ref_path, os.path.getsize(ref_path))
    tgt = host_bytes(tgt_path, vb)
    # resident reference (untimed upload)
    t = time.perf_counter()
    S = [torch.from_numpy(ref[o:o + n].view(np.int16).copy()).to(dev) for o, n in zip(range(0, len(ref), PIECE), pieces_of(len(ref)))]
    sync()
    res["ref_upload_s"] = time.perf_counter() - t
    al = (vb // BLK) * BLK  # the device holds the block-aligned part; the tail (< 64 B) stays on the host
    D = [torch.empty(n // 2, dtype=torch.int16, device=dev) for n in pieces_of(al)]
    # baseline: upload the whole target
    host_t = [torch.from_numpy(tgt[o:o + n].view(np.int16).copy()) for o, n in zip(range(0, al, PIECE), pieces_of(al))]
    sync()
    t = time.perf_counter()
    for d, h in zip(D, host_t):
        d.copy_(h)
    sync()
    res["baseline_upload_s"] = time.perf_counter() - t
    # patch: every block not covered by an even match run
    cover = np.zeros(vb // BLK + 1, dtype=bool)
    for d0, s0, l, c, dd, ds in g:
        for k in range(c):
            a = (d0 + k * dd) // BLK
            cover[a:a + l // BLK] = True
    need = ~cover[: vb // BLK]
    need[r["other"][r["other"] < vb // BLK]] = True
    for d0, l in gone:  # absorbed runs: the segment's copy wrote over them
        need[d0 // BLK:(d0 + l) // BLK] = True
    patch_blocks = np.nonzero(need)[0]
    pb = tgt[: (vb // BLK) * BLK].reshape(-1, BLK)[patch_blocks].copy()
    by_piece = {}
    for i, b in enumerate(patch_blocks.tolist()):
        by_piece.setdefault((b * BLK) // PIECE, []).append(i)
    res["patch_blocks"] = int(len(patch_blocks))
    res["patch_bytes"] = int(len(patch_blocks) * BLK)
    tail = tgt[(vb // BLK) * BLK:vb]
    # clear the target so the replay cannot pass by accident
    for d in D:
        d.zero_()
    sync()
    path = {"strided": 0, "single": 0, "fallback": 0}
    t0 = time.perf_counter()
    for d0, s0, l, c, dd, ds in g:
        dp, do = divmod(d0, PIECE)
        sp, so = divmod(s0, PIECE)
        le = l // 2
        if c == 1:
            D[dp][do // 2:do // 2 + le].copy_(S[sp][so // 2:so // 2 + le])
            path["single"] += 1
            continue
        try:
            D[dp].as_strided((c, le), (dd // 2, 1), do // 2).copy_(S[sp].as_strided((c, le), (ds // 2, 1), so // 2))
            path["strided"] += 1
        except Exception:
            for k in range(c):
                a, b = (do + k * dd) // 2, (so + k * ds) // 2
                D[dp][a:a + le].copy_(S[sp][b:b + le])
            path["fallback"] += 1
    sync()
    t1 = time.perf_counter()
    for p, rows in by_piece.items():
        sel = np.array(rows)
        local = (patch_blocks[sel] * BLK - p * PIECE) // BLK
        buf = torch.from_numpy(pb[sel].view(np.int16)).to(dev)
        idx = torch.from_numpy(local.astype(np.int64)).to(dev)
        D[p].view(-1, BLK // 2).index_copy_(0, idx, buf)
    sync()
    t2 = time.perf_counter()
    res.update(copy_s=t1 - t0, patch_s=t2 - t1, rebuild_s=t2 - t0, paths=path)
    # verify
    h = hashlib.sha256()
    got = np.concatenate([d.cpu().numpy().view(np.uint8) for d in D])[: (vb // BLK) * BLK]
    h.update(got.tobytes())
    h.update(tail.tobytes())
    want = hashlib.sha256(tgt[:vb].tobytes()).hexdigest()
    res.update(sha256=h.hexdigest(), target_sha256=want, correct=h.hexdigest() == want)
    res["mismatch_bytes"] = int((got != tgt[: (vb // BLK) * BLK]).sum()) if not res["correct"] else 0
    with open(out, "w") as f:
        json.dump(res, f, indent=1)
    print(json.dumps({k: v for k, v in res.items() if k != "load"}))


WIN = int(os.environ.get("REPLAY_WIN", str(64 << 20)))     # bytes of target written per window
RING = int(os.environ.get("REPLAY_RING", str(256 << 20)))  # bytes of original content kept behind the window


def replay_inplace(runs_path, out, platform):
    """Rebuild the target in the memory that holds the sibling, with O(RING) scratch.

    One buffer holds the sibling's bytes and is overwritten with the target window by window, in
    target order. Before a window is written, its original bytes are saved in a ring of
    RING/WIN + 1 slots. A copy reads a source byte from:
      - the live buffer if it lies after the current window (not yet overwritten);
      - the ring if it lies in the RING bytes before the window, or in the window itself;
      - a far scratch, filled before the first write, for the few sources further back.
    Invariant: no original byte is overwritten before it is saved, and every copy that reads the
    live buffer reads bytes after the current window. The patch is applied last."""
    import torch
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    from npu_d2d_bench import device
    r, dst, src, ln, odd_d, odd_l = load(runs_path)
    lim = ABSORB if "REPLAY_ABSORB" in os.environ else absorb_threshold(platform)
    dst, src, ln, gone = absorb(dst, src, ln, limit=lim)
    dst, src, ln = coalesce(dst, src, ln)
    dev, sync, pk = device(platform)
    ref_path, tgt_path = str(r["refs"][0]), str(r["target"])
    vb = int(r["v_bytes"])
    al = (vb // BLK) * BLK
    ref = np.fromfile(ref_path, dtype=np.uint8)
    tgt = np.fromfile(tgt_path, dtype=np.uint8)
    total = max(len(ref), al)
    total += (-total) % WIN
    nslots = RING // WIN + 1
    pieces = []
    for d, s_, l in zip(dst.tolist(), src.tolist(), ln.tolist()):
        while l > 0:
            take = min(l, WIN - d % WIN)
            pieces.append((d, s_, take))
            d, s_, l = d + take, s_ + take, l - take
    pieces.sort()
    kept = [x for x in pieces if x[0] == x[1]]          # KEEP: already in place, no copy
    pieces_cp = [x for x in pieces if x[0] != x[1]]
    segs = []
    far = []
    far_bytes = 0
    need_ring = set()        # windows whose original bytes a later copy reads: only these are saved
    for d, s_, l in pieces_cp:
        w = d // WIN
        a = w * WIN
        ring_lo = max(a - (nslots - 1) * WIN, 0)
        x, dd = s_, d
        while x < s_ + l:
            if x >= a + WIN:
                take = min(s_ + l - x, PIECE - x % PIECE)
                segs.append((w, dd, "live", x // PIECE, x % PIECE, take))
            elif x >= ring_lo:
                take = min(s_ + l - x, WIN - x % WIN, a + WIN - x)
                segs.append((w, dd, "ring", (x // WIN) % nslots, x % WIN, take))
                need_ring.add(x // WIN)
            else:
                take = min(s_ + l - x, ring_lo - x, PIECE - (far_bytes % PIECE))
                segs.append((w, dd, "far", far_bytes // PIECE, far_bytes % PIECE, take))
                far.append((x, take))
                far_bytes += take
            x += take
            dd += take
    res = dict(schema="npu-replay-inplace.v1", platform=platform, device=dev, ref=ref_path, target=tgt_path,
               v_bytes=vb, win=WIN, ring=RING, slots=nslots, copies=len(segs), far_bytes=far_bytes,
               absorb_limit=lim, absorbed_bytes=int(sum(x[1] for x in gone)),
               keep_bytes=int(sum(x[2] for x in kept)), keep_runs=len(kept),
               far_copies=len(far), scratch_bytes=nslots * WIN + far_bytes, buffer_bytes=total,
               load=(os.getloadavg() if hasattr(os, "getloadavg") else None))
    host = np.zeros(total, dtype=np.uint8)
    host[:len(ref)] = ref
    Bf = [torch.from_numpy(host[o:o + PIECE].view(np.int16).copy()).to(dev) for o in range(0, total, PIECE)]
    ringt = [torch.empty(WIN // 2, dtype=torch.int16, device=dev) for _ in range(nslots)]
    fart = [torch.empty(min(PIECE, far_bytes - o) // 2, dtype=torch.int16, device=dev) for o in range(0, far_bytes, PIECE)]
    sync()
    del host
    # NPU-B: batched copies through the public runtime API (rbln_memcpy_v2v_multi / h2v_multi);
    # copies inside one call are unordered, so a call never mixes a window's ring save with its
    # copies, and no copy in a call writes where another reads
    shim = None
    if platform == "rbln" and os.environ.get("RBLN_VMSHIM") and os.environ.get("REPLAY_BATCH", "1") != "0":
        import ctypes
        shim = ctypes.CDLL(os.environ["RBLN_VMSHIM"])
        for t in ringt + fart:
            shim.vmshim_layout_like(ctypes.c_uint64(t.data_ptr()), ctypes.c_uint64(Bf[0].data_ptr()))
    if platform == "cpu" and os.environ.get("REPLAY_FAKE_BATCH"):   # test the batched addressing on the host
        import ctypes

        class _Fake:
            @staticmethod
            def _run(a_, b_, c_, n):
                for i in range(n):
                    ctypes.memmove(b_[i], a_[i], c_[i])
                return 0
            vmshim_v2v_multi = _run
            vmshim_h2v_multi = _run
        shim = _Fake()
    MAXB = 1024
    batch_calls = [0]

    def flush(ops, h2v=False):
        import ctypes
        for i in range(0, len(ops), MAXB):
            part = ops[i:i + MAXB]
            U = ctypes.c_uint64 * len(part)
            a_, b_, c_ = U(*[x[0] for x in part]), U(*[x[1] for x in part]), U(*[x[2] for x in part])
            rc = (shim.vmshim_h2v_multi if h2v else shim.vmshim_v2v_multi)(a_, b_, c_, len(part))
            if rc != 0:
                raise RuntimeError("batched copy rc=%d" % rc)
            batch_calls[0] += 1
    need = np.ones(al // BLK, dtype=bool)
    for d, s_, l in pieces:
        need[d // BLK:(d + l) // BLK] = False
    need[r["other"][r["other"] < al // BLK]] = True
    for d0, l in gone:
        need[d0 // BLK:(d0 + l) // BLK] = True
    # runs at odd offsets cannot be addressed in 16-bit device elements: their blocks are uploaded
    for d0, l in zip(odd_d.tolist(), odd_l.tolist()):
        need[d0 // BLK:(d0 + l + BLK - 1) // BLK] = True
    res["odd_runs"] = int(len(odd_d))
    res["odd_bytes"] = int(odd_l.sum())
    patch_blocks = np.nonzero(need)[0]
    pb = tgt[:al].reshape(-1, BLK)[patch_blocks].copy()
    res.update(patch_blocks=int(len(patch_blocks)), patch_bytes=int(len(patch_blocks) * BLK))
    nwin = (max(x[0] for x in pieces) // WIN + 1) if pieces else 0
    n_copies = len(segs) + len(far) + len(need_ring)
    copy_bytes = sum(sg[5] for sg in segs) + sum(l for _, l in far) + len(need_ring) * WIN
    res.update(windows=int(nwin), ring_saves=len(need_ring), relocate_bytes=int(sum(sg[5] for sg in segs)))
    est_rebuild, est_upload = estimate(platform, n_copies, copy_bytes, res["patch_bytes"], al)
    in_host = os.environ.get("REPLAY_TARGET_IN_HOST") == "1"
    # the far scratch is known before the first write; beyond its budget the target is uploaded
    far_budget = int(os.environ.get("REPLAY_FAR_BUDGET", str(1 << 30)))
    res.update(est_rebuild_s=est_rebuild, est_upload_s=est_upload, target_in_host=in_host, far_budget=far_budget,
               choice=("upload" if (in_host and est_upload < est_rebuild) or far_bytes > far_budget else "rebuild"))

    def E(b):
        return b // 2

    def view(origin, key, off, l):
        t = {"live": Bf, "ring": ringt, "far": fart}[origin][key]
        return t[E(off):E(off) + E(l)]
    by_w = {}
    for sg in segs:
        by_w.setdefault(sg[0], []).append(sg)
    # fold, per window: copies from one source tensor with equal length and constant target and
    # source strides become one strided copy (as_strided on both sides)
    plan = {}
    nfold = 0
    for w, lst in by_w.items():
        lst.sort(key=lambda x: x[1])
        out_w, i = [], 0
        while i < len(lst):
            _, d0, o0, k0, f0, l0 = lst[i]
            j = i + 1
            if j < len(lst) and lst[j][2] == o0 and lst[j][3] == k0 and lst[j][5] == l0:
                dd, ds = lst[j][1] - d0, lst[j][4] - f0
                if dd >= l0 and ds > 0 and d0 // PIECE == lst[j][1] // PIECE:
                    while (j + 1 < len(lst) and lst[j + 1][2] == o0 and lst[j + 1][3] == k0 and lst[j + 1][5] == l0
                           and lst[j + 1][1] - lst[j][1] == dd and lst[j + 1][4] - lst[j][4] == ds
                           and lst[j + 1][1] // PIECE == d0 // PIECE):
                        j += 1
                    j += 1
                else:
                    j = i + 1
            out_w.append((d0, o0, k0, f0, l0, j - i, (lst[i + 1][1] - d0) if j - i > 1 else 0,
                          (lst[i + 1][4] - f0) if j - i > 1 else 0))
            nfold += 1
            i = j
        plan[w] = out_w
    res["calls"] = nfold
    path = {"strided": 0, "single": 0, "fallback": 0}
    t0 = time.perf_counter()
    fo = 0
    if shim is not None:
        ops = []
        for x, l in far:
            ops.append((Bf[x // PIECE].data_ptr() + x % PIECE, fart[fo // PIECE].data_ptr() + fo % PIECE, l))
            fo += l
        flush(ops)
        last_w = max(plan) if plan else -1
        for w in range(0, last_w + 1):
            a = w * WIN
            if w in need_ring:
                flush([(Bf[a // PIECE].data_ptr() + a % PIECE, ringt[w % nslots].data_ptr(), WIN)])
            ops = []
            for d, origin, key, off, l, c, dd, ds in plan.get(w, []):
                st = {"live": Bf, "ring": ringt, "far": fart}[origin][key]
                for k in range(c):
                    ops.append((st.data_ptr() + off + k * ds, Bf[d // PIECE].data_ptr() + d % PIECE + k * dd, l))
            flush(ops)
        path["batched"] = batch_calls[0]
        plan = {}
    for x, l in far if shim is None else []:
        fart[fo // PIECE][E(fo % PIECE):E(fo % PIECE) + E(l)].copy_(Bf[x // PIECE][E(x % PIECE):E(x % PIECE) + E(l)])
        fo += l
    last_w = max(plan) if plan else -1
    for w in range(0, last_w + 1):
        a = w * WIN
        if w in need_ring:
            ringt[w % nslots].copy_(Bf[a // PIECE][E(a % PIECE):E(a % PIECE) + E(WIN)])
        for d, origin, key, off, l, c, dd, ds in plan.get(w, []):
            dt = Bf[d // PIECE]
            st = {"live": Bf, "ring": ringt, "far": fart}[origin][key]
            if c == 1:
                dt[E(d % PIECE):E(d % PIECE) + E(l)].copy_(st[E(off):E(off) + E(l)])
                path["single"] += 1
                continue
            try:
                dt.as_strided((c, E(l)), (E(dd), 1), E(d % PIECE)).copy_(st.as_strided((c, E(l)), (E(ds), 1), E(off)))
                path["strided"] += 1
            except Exception:
                for k in range(c):
                    a2, b2 = E(d % PIECE + k * dd), E(off + k * ds)
                    dt[a2:a2 + E(l)].copy_(st[b2:b2 + E(l)])
                path["fallback"] += 1
    sync()
    res["paths"] = path
    t1 = time.perf_counter()
    if shim is not None and len(patch_blocks):
        brk = np.nonzero(np.diff(patch_blocks) != 1)[0] + 1
        starts = np.concatenate([[0], brk])
        ends = np.concatenate([brk, [len(patch_blocks)]])
        ops = []
        base = pb.ctypes.data
        for a_, b_ in zip(starts.tolist(), ends.tolist()):
            off = int(patch_blocks[a_]) * BLK
            n_ = (b_ - a_) * BLK
            while n_ > 0:                                  # do not cross a device piece
                take = min(n_, PIECE - off % PIECE)
                ops.append((base + a_ * BLK, Bf[off // PIECE].data_ptr() + off % PIECE, take))
                a_ += take // BLK
                off += take
                n_ -= take
        res["patch_ranges"] = len(ops)
        flush(ops, h2v=True)
        path["batched"] = batch_calls[0]
    else:
        by_piece = {}
        for i, b in enumerate(patch_blocks.tolist()):
            by_piece.setdefault((b * BLK) // PIECE, []).append(i)
        for p_, rows in by_piece.items():
            sel = np.array(rows)
            local = (patch_blocks[sel] * BLK - p_ * PIECE) // BLK
            Bf[p_].view(-1, BLK // 2).index_copy_(0, torch.from_numpy(local.astype(np.int64)).to(dev),
                                                   torch.from_numpy(pb[sel].view(np.int16)).to(dev))
    sync()
    t2 = time.perf_counter()
    got = np.concatenate([b.cpu().numpy().view(np.uint8) for b in Bf])[:al]
    want = tgt[:al]
    ok = bool(np.array_equal(got, want))
    res.update(copy_s=t1 - t0, patch_s=t2 - t1, rebuild_s=t2 - t0, correct=ok,
               mismatch_bytes=0 if ok else int((got != want).sum()),
               sha256=hashlib.sha256(got.tobytes() + tgt[al:vb].tobytes()).hexdigest(),
               target_sha256=hashlib.sha256(tgt[:vb].tobytes()).hexdigest())
    del got
    if res["target_in_host"]:
        # the other path: upload the whole target from host memory into the same buffer
        hostp = [torch.from_numpy(tgt[o:min(o + PIECE, al)].view(np.int16).copy()) for o in range(0, al, PIECE)]
        sync()
        t3 = time.perf_counter()
        for dst_t, src_t in zip(Bf, hostp):
            dst_t[:src_t.numel()].copy_(src_t)
        sync()
        res["upload_s"] = time.perf_counter() - t3
        res["upload_correct"] = bool(np.array_equal(np.concatenate([b.cpu().numpy().view(np.uint8) for b in Bf])[:al], want))
        res["measured_choice"] = "upload" if res["upload_s"] < res["rebuild_s"] else "rebuild"
    with open(out, "w") as f:
        json.dump(res, f, indent=1)
    print(json.dumps({k: v for k, v in res.items() if k != "load"}))


if __name__ == "__main__":
    if sys.argv[1] == "analyze":
        analyze(sys.argv[2], sys.argv[3])
    elif sys.argv[1] == "inplace":
        replay_inplace(sys.argv[2], sys.argv[3], sys.argv[4])
    else:
        replay(sys.argv[2], sys.argv[3], sys.argv[4])
