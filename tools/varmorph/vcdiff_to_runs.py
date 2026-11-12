#!/usr/bin/env python3
"""Turn an xdelta3 (VCDIFF) patch into the copy list that the device rebuild reads (runs.npz).

Reads `xdelta3 printdelta PATCH` on stdin. printdelta prints the target offset and S@a
(source offset) as absolute values, and T@a relative to the start of the current VCDIFF window
("window at offset"); a T@ copy repeats bytes already decoded in that window. A copy from the target (T@) is resolved to the source bytes it repeats,
piece by piece, so the result is generous to the delta: every byte a copy chain carries from the
source becomes a run. ADD and RUN bytes, and copies that resolve to them, have no source and go
to the patch. Runs are trimmed to whole 64-byte target blocks; blocks not fully covered form
`other`. The instruction counts of the patch as written are stored too.
argv: OUT.npz REF TARGET < printdelta.txt
"""
import bisect
import json
import os
import sys

import numpy as np

BLK = 64


class Window:
    def __init__(self, t0, src0):
        self.t0, self.src0 = t0, src0
        self.st, self.en, self.kind, self.src = [], [], [], []

    def emit(self, t, l, kind, s):
        if l <= 0:
            return
        if self.st and self.en[-1] == t and self.kind[-1] == kind and \
                (kind == 0 or self.src[-1] + (self.en[-1] - self.st[-1]) == s):
            self.en[-1] = t + l
            return
        self.st.append(t)
        self.en.append(t + l)
        self.kind.append(kind)
        self.src.append(s)


def main():
    out, ref, target = sys.argv[1], sys.argv[2], sys.argv[3]
    vb = os.path.getsize(target)
    st = dict(windows=0, cpy_s=0, cpy_t=0, add=0, run=0, cpy_s_bytes=0, cpy_t_bytes=0, add_bytes=0, run_bytes=0)
    rd, rs, rl = [], [], []
    w = None
    next_t0, cw_off, tlen, win_at = 0, 0, 0, 0

    def close(win):
        if win is None:
            return
        for a, e, k, s in zip(win.st, win.en, win.kind, win.src):
            if k == 1:
                rd.append(win.t0 + a)
                rs.append(win.src0 + s)
                rl.append(e - a)

    for line in sys.stdin:
        if line.startswith("VCDIFF window number"):
            close(w)
            w = None
            st["windows"] += 1
            continue
        if line.startswith("VCDIFF window at offset"):
            win_at = int(line.split(":")[1])
            continue
        if line.startswith("VCDIFF copy window offset"):
            cw_off = int(line.split(":")[1])
            continue
        if line.startswith("VCDIFF target window length"):
            tlen = int(line.split(":")[1])
            w = Window(0, 0)            # printdelta prints absolute target offsets and addresses
            next_t0 += tlen
            cw_off = 0
            continue
        f = line.split()
        if w is None or len(f) < 4 or not f[0].isdigit():
            continue
        t = int(f[0])
        rest = f[2:]
        while rest:
            kind = rest[0]
            if kind in ("ADD", "RUN"):
                size, addr, rest = int(rest[1]), None, rest[2:]
                st["add" if kind == "ADD" else "run"] += 1
                st[("add" if kind == "ADD" else "run") + "_bytes"] += size
                w.emit(t, size, 0, 0)
            else:
                size, addr, rest = int(rest[1]), rest[2], rest[3:]
                if addr.startswith("S@"):
                    st["cpy_s"] += 1
                    st["cpy_s_bytes"] += size
                    w.emit(t, size, 1, int(addr[2:]))
                else:
                    st["cpy_t"] += 1
                    st["cpy_t_bytes"] += size
                    p, done = win_at + int(addr[2:]), 0      # T@ is relative to the window
                    while done < size:
                        i = bisect.bisect_right(w.st, p) - 1
                        avail = min(w.en[i] - p, size - done, (t + done) - p)
                        if w.kind[i] == 1:
                            w.emit(t + done, avail, 1, w.src[i] + (p - w.st[i]))
                        else:
                            w.emit(t + done, avail, 0, 0)
                        p += avail
                        done += avail
            t += size
    close(w)
    d = np.array(rd, np.int64)
    s = np.array(rs, np.int64)
    l = np.array(rl, np.int64)
    st["source_runs_raw"] = int(len(d))
    st["source_bytes"] = int(l.sum())
    # merge across windows: consecutive runs with the same move
    o = np.argsort(d, kind="stable")
    d, s, l = d[o], s[o], l[o]
    md, ms, ml = [], [], []
    for a, b, c in zip(d.tolist(), s.tolist(), l.tolist()):
        if md and md[-1] + ml[-1] == a and ms[-1] + ml[-1] == b:
            ml[-1] += c
        else:
            md.append(a), ms.append(b), ml.append(c)
    d, s, l = np.array(md, np.int64), np.array(ms, np.int64), np.array(ml, np.int64)
    st["source_runs_merged"] = int(len(d))
    # trim to whole target blocks
    a = (d + BLK - 1) // BLK * BLK
    e = (d + l) // BLK * BLK
    keep = e > a
    s = s + (a - d)
    d, s, l = a[keep], s[keep], (e - a)[keep]
    al = vb // BLK
    covered = np.zeros(al, dtype=bool)
    for a_, l_ in zip(d.tolist(), l.tolist()):
        covered[a_ // BLK:(a_ + l_) // BLK] = True
    other = np.nonzero(~covered)[0].astype(np.int64)
    R = np.memmap(ref, dtype=np.uint8, mode="r")
    T = np.memmap(target, dtype=np.uint8, mode="r")
    bad = sum(1 for a_, b_, l_ in zip(d.tolist(), s.tolist(), l.tolist())
              if not np.array_equal(R[b_:b_ + l_], T[a_:a_ + l_]))
    st["runs_mismatched"] = int(bad)
    st["runs"] = int(len(d))
    st["run_bytes_trimmed"] = int(l.sum())
    st["other_blocks"] = int(len(other))
    st["v_bytes"] = vb
    np.savez(out, dst=d, src=s, len=l, other=other, other_mode=np.zeros(len(other), np.uint8),
             v_bytes=np.int64(vb), refs=np.array([ref]), target=np.array(target))
    with open(out + ".json", "w") as fh:
        json.dump(st, fh, indent=1)
    print(json.dumps(st))


if __name__ == "__main__":
    main()
