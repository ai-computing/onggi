#!/usr/bin/env python3
"""Tensor-level dedup baseline (ZipLLM-style) for the paper (#73).

    bench/tensor_dedup.py [--workers N] [--level 3] [--follow-symlinks] <path>... > result.json

safetensors files are split into tensors by their JSON header (8-byte little-endian length, then JSON); each
tensor is an object keyed by the sha256 of its bytes. Every other file is one opaque object keyed by the sha256 of
the whole file: compiled NPU artifacts (.rbln and the like) are not parsed (onggi ADR-0009, vendor formats stay
opaque), so on them this baseline is whole-file dedup. Each distinct object is stored once, compressed with
`zstd -<level>` (the zstd CLI, one thread per object). safetensors headers are stored as they are.

Stored bytes = compressed distinct objects + safetensors headers + the metadata the store would need to restore
every file (a manifest listing each file's objects and an object index), counted as compact JSON. Paths are walked in
sorted order. Needs Python 3.8+ and zstd on PATH; reads the input, writes nothing but stdout.
"""
import argparse
import hashlib
import json
import os
import struct
import subprocess
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor

BUF = 8 << 20


def zstd_size(level, feed):
    """Compress what feed(write) writes with the zstd CLI; return the compressed size."""
    p = subprocess.Popen(["zstd", "-q", "-%d" % level, "-T1", "-c"], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    n = [0]

    def count():
        while True:
            b = p.stdout.read(BUF)
            if not b:
                return
            n[0] += len(b)

    t = threading.Thread(target=count)
    t.start()
    try:
        feed(p.stdin.write)
    finally:
        p.stdin.close()
        t.join()
    if p.wait() != 0:
        raise RuntimeError("zstd failed")
    return n[0]


def read_range(f, off, length, write, hasher):
    f.seek(off)
    left = length
    while left:
        b = f.read(min(BUF, left))
        if not b:
            raise IOError("short read")
        hasher.update(b)
        write(b)
        left -= len(b)


class Store:
    def __init__(self, level):
        self.level = level
        self.lock = threading.Lock()
        self.objects = {}  # sha256 hex -> [raw size, stored size]
        self.dup_objects = 0
        self.dup_bytes = 0

    def add(self, digest, raw, stored):
        """Record an object; return True if it was new."""
        with self.lock:
            if digest in self.objects:
                self.dup_objects += 1
                self.dup_bytes += raw
                return False
            self.objects[digest] = [raw, stored]
            return True

    def seen(self, digest):
        with self.lock:
            return digest in self.objects


def safetensors_header(path):
    with open(path, "rb") as f:
        head = f.read(8)
        if len(head) != 8:
            return None
        (n,) = struct.unpack("<Q", head)
        if n <= 1 or n > 100 << 20:
            return None
        raw = f.read(n)
    try:
        hdr = json.loads(raw.decode("utf-8"))
    except ValueError:
        return None
    return 8 + n, hdr


def do_safetensors(path, store, parsed):
    data_start, hdr = parsed
    row = {"kind": "safetensors", "header_bytes": data_start, "tensors": 0, "objects": []}
    tensors = sorted(
        ((k, v) for k, v in hdr.items() if k != "__metadata__"), key=lambda kv: kv[1]["data_offsets"][0]
    )
    with open(path, "rb") as f:
        for name, info in tensors:
            a, b = info["data_offsets"]
            h = hashlib.sha256()
            # hash first (cheap) so duplicates are not compressed
            read_range(f, data_start + a, b - a, lambda _b: None, h)
            d = h.hexdigest()
            stored = 0
            if not store.seen(d):
                stored = zstd_size(store.level, lambda w: read_range(f, data_start + a, b - a, w, hashlib.sha256()))
            new = store.add(d, b - a, stored)
            row["tensors"] += 1
            row["objects"].append(d)
            row.setdefault("new_tensors", 0)
            row["new_tensors"] += 1 if new else 0
    return row


def do_opaque(path, size, store):
    h = hashlib.sha256()
    # one pass: hash and compress together; a duplicate's compressed size is dropped
    with open(path, "rb") as f:
        stored = zstd_size(store.level, lambda w: read_range(f, 0, size, w, h))
    d = h.hexdigest()
    store.add(d, size, stored)
    return {"kind": "opaque", "objects": [d]}


def walk(paths, follow):
    out = []
    for p in paths:
        if os.path.isfile(p):
            out.append((p, os.path.basename(p)))
            continue
        for root, dirs, files in os.walk(p, followlinks=follow):
            dirs.sort()
            for n in sorted(files):
                full = os.path.join(root, n)
                if os.path.islink(full) and not follow:
                    continue
                out.append((full, os.path.relpath(full, p)))
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("paths", nargs="+")
    ap.add_argument("--workers", type=int, default=8, help="files processed at once (default 8)")
    ap.add_argument("--level", type=int, default=3, help="zstd level (default 3)")
    ap.add_argument("--follow-symlinks", action="store_true", help="follow symlinks (e.g. a Hugging Face snapshot)")
    args = ap.parse_args()

    files = walk(args.paths, args.follow_symlinks)
    store = Store(args.level)
    t0 = time.time()

    def one(item):
        full, rel = item
        size = os.path.getsize(full)
        t = time.time()
        parsed = safetensors_header(full) if full.endswith(".safetensors") else None
        row = do_safetensors(full, store, parsed) if parsed else do_opaque(full, size, store)
        row.update(path=full, name=rel, size=size, seconds=round(time.time() - t, 2))
        print("done %s %.1fs" % (rel, row["seconds"]), file=sys.stderr, flush=True)
        return row

    with ThreadPoolExecutor(max_workers=args.workers) as ex:
        rows = list(ex.map(one, files))

    manifest = [{"name": r["name"], "size": r["size"], "objects": r["objects"]} for r in rows]
    index = [[d, v[0], v[1]] for d, v in sorted(store.objects.items())]
    meta = len(json.dumps(manifest, separators=(",", ":"))) + len(json.dumps(index, separators=(",", ":")))
    headers = sum(r.get("header_bytes", 0) for r in rows)
    objects_stored = sum(v[1] for v in store.objects.values())
    raw = sum(r["size"] for r in rows)
    for r in rows:
        r["objects"] = len(r["objects"])
    res = {
        "tool": "bench/tensor_dedup.py",
        "zstd_level": args.level,
        "files": len(rows),
        "raw_bytes": raw,
        "safetensors_files": sum(1 for r in rows if r["kind"] == "safetensors"),
        "opaque_files": sum(1 for r in rows if r["kind"] == "opaque"),
        "distinct_objects": len(store.objects),
        "distinct_raw_bytes": sum(v[0] for v in store.objects.values()),
        "duplicate_objects": store.dup_objects,
        "duplicate_bytes": store.dup_bytes,
        "objects_stored_bytes": objects_stored,
        "safetensors_header_bytes": headers,
        "metadata_bytes": meta,
        "stored_bytes": objects_stored + headers + meta,
        "ratio": round(raw / float(objects_stored + headers + meta), 3) if raw else None,
        "seconds": round(time.time() - t0, 1),
        "per_file": rows,
    }
    json.dump(res, sys.stdout, indent=1)
    print()


if __name__ == "__main__":
    main()
