#!/usr/bin/env python3
"""Cohort 2026-11-12 (11): what a compiled binary holds, by content class, without parsing its format.

Every 64 KiB window of a file is classified from byte statistics alone, so the same rule applies to
any vendor's artifact:
  fill     one or two distinct byte values (padding, zero-initialized buffers)
  text     >= 85 % printable ASCII (metadata, names, JSON)
  fp16/bf16  16-bit float weights: >= 90 % of 16-bit words have a high byte in 0x04-0x3F or
           0x84-0xBF (|x| < 2 as fp16 or bf16), the two signs are balanced within 10 %, and the
           high byte has <= 5.5 bits of entropy. The median magnitude byte tells the two apart
           (bf16 weights sit at 0x3A-0x3D, fp16 weights below 0x38).
  fp32     the same test on the top byte of 32-bit words (fp32 tensors: norms, tables, biases);
           every byte phase is tried, since tensors start at odd offsets in some containers
  bf16_in_fp32  fp32 words whose low 16 bits are zero in >= 95 % of words: bf16 values widened
  code     byte entropy < 7.0 bits (instructions, tables, descriptors; structured, compressible)
  dense    the rest (high entropy, not weight-like: packed or quantized data, compressed sections)
Per class: bytes, mean byte entropy, mean entropy of the high and low byte of 16-bit words.

Optional join with a coding result: MODES.npy holds, per 64 KiB window of the target, the number of
64-byte blocks coded as match / fill / residual / intra (from vcodec_server.py). With it, the
output also gives how each class of the target was found in its sibling.
argv: OUT.json FILE [FILE ...] [--modes FILE=MODES.npy ...]
"""
import json
import os
import sys
import time

import numpy as np

WIN = 1 << 16
CLASSES = ["fill", "text", "bf16", "fp32", "code", "dense", "fp16", "bf16_in_fp32"]


def entropy_rows(counts):
    tot = counts.sum(axis=1, keepdims=True).astype(np.float64)
    p = counts / np.maximum(tot, 1)
    with np.errstate(divide="ignore", invalid="ignore"):
        e = -(np.where(p > 0, p * np.log2(p), 0.0)).sum(axis=1)
    return e


def hist_rows(a, nw):
    """Per-row 256-bin histogram of a (nw, m) uint8 array."""
    off = (np.arange(nw, dtype=np.int64) * 256)[:, None]
    return np.bincount((a.astype(np.int64) + off).ravel(), minlength=nw * 256).reshape(nw, 256)


def classify(path):
    n = os.path.getsize(path) // WIN
    mm = np.memmap(path, dtype=np.uint8, mode="r", shape=(n * WIN,))
    cls = np.empty(n, dtype=np.uint8)
    feats = np.empty((n, 3), dtype=np.float32)
    step = (1 << 28) // WIN
    wl_hi = np.zeros(256, dtype=bool)
    wl_hi[0x04:0x40] = True
    wl_hi[0x84:0xC0] = True
    printable = np.zeros(256, dtype=bool)
    printable[0x20:0x7F] = True
    printable[[9, 10, 13]] = True
    for i in range(0, n, step):
        j = min(n, i + step)
        a = np.asarray(mm[i * WIN:j * WIN]).reshape(j - i, WIN)
        nw = j - i
        hb = hist_rows(a, nw)
        h_odd = hist_rows(a[:, 1::2], nw)      # high byte of 16-bit words at phase 0
        h_even = hist_rows(a[:, 0::2], nw)     # high byte of 16-bit words at phase 1
        e = entropy_rows(hb)
        distinct = (hb > 0).sum(axis=1)
        asc = (hb[:, printable].sum(axis=1)) / WIN

        def test16(hh):
            w = hh[:, wl_hi].sum(axis=1) / (WIN // 2)
            sym = 1 - np.abs(hh[:, :128].sum(axis=1) - hh[:, 128:].sum(axis=1)) / (WIN // 2)
            mag = hh[:, :128] + hh[:, 128:]
            med = (np.cumsum(mag, axis=1) >= (WIN // 4)).argmax(axis=1)
            return (w >= 0.9) & (sym >= 0.9) & (entropy_rows(hh) <= 5.5), med, entropy_rows(hh)
        ok0, med0, e0 = test16(h_odd)
        ok1, med1, e1 = test16(h_even)
        is16 = ok0 | ok1
        med = np.where(ok0, med0, med1)
        ehi = np.where(ok1 & ~ok0, e1, e0)
        elo = np.where(ok1 & ~ok0, e0, e1)
        is32 = np.zeros(nw, dtype=bool)
        zlow = np.zeros(nw)
        for p in range(4):
            h32 = hist_rows(a[:, p::4], nw)
            w32 = h32[:, wl_hi].sum(axis=1) / (WIN // 4)
            sym32 = 1 - np.abs(h32[:, :128].sum(axis=1) - h32[:, 128:].sum(axis=1)) / (WIN // 4)
            ok = (w32 >= 0.9) & (sym32 >= 0.9)
            if ok.any():
                lo = (hist_rows(a[:, (p - 3) % 4::4], nw)[:, 0] + hist_rows(a[:, (p - 2) % 4::4], nw)[:, 0]) / (WIN // 2)
                zlow = np.where(ok & ~is32, lo, zlow)
            is32 |= ok
        c = np.full(nw, 5, dtype=np.uint8)
        c[e < 7.0] = 4
        c[is32 & ~is16] = 3
        c[is32 & ~is16 & (zlow >= 0.95)] = 7          # fp32 holding bf16 values (low half zero)
        c[is16 & (med >= 0x38)] = 2
        c[is16 & (med < 0x38)] = 6
        c[asc >= 0.85] = 1
        c[distinct <= 2] = 0
        cls[i:j] = c
        feats[i:j, 0] = e
        feats[i:j, 1] = ehi
        feats[i:j, 2] = elo
    return cls, feats


def summary(path, cls, feats, modes=None):
    out = {"file": path, "bytes": os.path.getsize(path), "windows": int(len(cls)), "classes": {}}
    for k, name in enumerate(CLASSES):
        sel = cls == k
        if not sel.any():
            continue
        row = {"bytes": int(sel.sum()) * WIN, "share": float(sel.mean()),
               "entropy": float(feats[sel, 0].mean()), "entropy_hi": float(feats[sel, 1].mean()),
               "entropy_lo": float(feats[sel, 2].mean())}
        if modes is not None:
            m = modes[: len(cls)][sel[: len(modes)]].sum(axis=0).astype(np.int64) * 64
            row["coded_as"] = {"match": int(m[0]), "fill": int(m[1]), "residual": int(m[2]), "intra": int(m[3])}
        out["classes"][name] = row
    # contiguous extents of one class, to show the layout
    brk = np.nonzero(np.diff(cls.astype(np.int16)))[0] + 1
    st = np.concatenate([[0], brk])
    en = np.concatenate([brk, [len(cls)]])
    ext = [{"class": CLASSES[cls[s]], "offset": int(s) * WIN, "bytes": int(e - s) * WIN} for s, e in zip(st, en)]
    big = sorted(ext, key=lambda x: -x["bytes"])[:12]
    out["extents"] = int(len(ext))
    out["largest_extents"] = sorted(big, key=lambda x: x["offset"])
    return out


if __name__ == "__main__":
    args = sys.argv[1:]
    out = args[0]
    files, modes = [], {}
    k = 1
    while k < len(args):
        if args[k] == "--modes":
            f, m = args[k + 1].split("=", 1)
            modes[f] = m
            k += 2
        else:
            files.append(args[k])
            k += 1
    res = []
    for f in files:
        t = time.time()
        cls, feats = classify(f)
        md = np.load(modes[f]) if f in modes else None
        res.append(summary(f, cls, feats, md))
        res[-1]["seconds"] = round(time.time() - t, 1)
        with open(out, "w") as fh:
            json.dump(res, fh, indent=1)
        print(time.strftime("%H:%M:%S"), f, {c: round(v["share"], 3) for c, v in res[-1]["classes"].items()}, flush=True)
    print("ALL-DONE", flush=True)
