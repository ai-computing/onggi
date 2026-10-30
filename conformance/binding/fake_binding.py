#!/usr/bin/env python3
# Copyright 2026 The onggi authors. SPDX-License-Identifier: Apache-2.0
"""Reference device binding (ADR-0010) on a simulated device: a directory that holds the resident
artifact's files, updated in rows of 4 KiB. It shows the contract, not a runtime; Python 3.6+,
standard library only.

    fake_binding.py describe ARTIFACT_DIR          -> onggi.profile.v1
    fake_binding.py resident                       -> onggi.resident.v1
    fake_binding.py plan < switchplan.json         -> onggi.updateplan.v1 (no device side effects)
    fake_binding.py apply --target DIR < updateplan.json -> onggi.verification.v1
    fake_binding.py rollback                       -> onggi.verification.v1
    fake_binding.py conformance-load ARTIFACT_DIR MANIFEST   (kit hook: load a base the normal way)

The device directory is $ONGGI_FAKE_DEVICE (default ./.fake-device). ONGGI_FAKE_LEVEL=B1 makes plan
always answer reload, as a B1 binding does.
"""
import hashlib
import json
import os
import shutil
import sys

ROW = 4096  # the device unit: a "row"
MAX_SHARE = 0.25  # more of the resident bytes to rewrite than this: reload instead
MAX_RUNS = 64  # more changed runs than this: reload instead

DEV = os.environ.get("ONGGI_FAKE_DEVICE", ".fake-device")
MEM, STATE, JOURNAL = os.path.join(DEV, "mem"), os.path.join(DEV, "state.json"), os.path.join(DEV, "journal")


def fail(msg, code=2):
    sys.stderr.write("fake-binding: %s\n" % msg)
    sys.exit(code)


def out(obj):
    json.dump(obj, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")


def state():
    try:
        with open(STATE) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {"manifest": None}


def save_state(s):
    tmp = STATE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(s, f)
    os.replace(tmp, STATE)


def files_of(root):
    res = {}
    for dp, _, fs in os.walk(root):
        for f in fs:
            p = os.path.join(dp, f)
            res[os.path.relpath(p, root).replace(os.sep, "/")] = p
    return res


def read(path, off, n):
    with open(path, "rb") as f:
        f.seek(off)
        return f.read(n)


def sha(b):
    return hashlib.sha256(b).hexdigest()


def describe(artifact):
    """The variant coordinate from documented plain metadata only (ADR-0009): here config.json."""
    prof = {"schema": "onggi.profile.v1", "device": "fake", "runtime": "onggi-fake-binding", "format": "bytes",
            "layout": "rows-4KiB"}
    try:
        with open(os.path.join(artifact, "config.json")) as f:
            prof["batch"] = int(json.load(f).get("batch", 0))
    except (OSError, ValueError):
        pass
    out(prof)


def resident():
    m = state()["manifest"]
    out({"schema": "onggi.resident.v1", "manifest": m, "state": "ready" if m else "empty"})


def reload_plan(sp, reason):
    return {"schema": "onggi.updateplan.v1", "base_manifest": sp.get("base_manifest"), "manifest": sp.get("manifest"),
            "mode": "reload", "reason": reason, "units": [], "bytes": sum(f["size"] for f in sp.get("files", []))}


def plan(sp):
    """Pure: reads the resident state, writes nothing."""
    if sp.get("schema") != "onggi.switchplan.v1":
        fail("unknown switch plan schema %r" % sp.get("schema"))
    if state()["manifest"] != sp.get("base_manifest"):
        return reload_plan(sp, "base not resident")
    if os.environ.get("ONGGI_FAKE_LEVEL") == "B1":
        return reload_plan(sp, "B1 binding: always reload")
    resident_files = files_of(MEM)
    units, runs, total = [], 0, 0
    for f in sp["files"]:
        if "ranges" not in f:
            return reload_plan(sp, "switch plan without ranges (onggi diff --ranges --json)")
        base = resident_files.get(f.get("base") or "")
        if f.get("base") != f["path"] or base is None or os.path.getsize(base) != f["size"]:
            return reload_plan(sp, "%s: not the same file and size on the device" % f["path"])
        total += f["size"]
        rows = set()
        for r in f["ranges"]:
            if r["kind"] == "same":
                continue
            runs += 1
            rows.update(range(r["offset"] // ROW, (r["offset"] + r["length"] - 1) // ROW + 1))
        for row in sorted(rows):  # merge adjacent rows into units
            off = row * ROW
            n = min(ROW, f["size"] - off)
            if units and units[-1]["file"] == f["path"] and units[-1]["offset"] + units[-1]["length"] == off:
                units[-1]["length"] += n
            else:
                units.append({"file": f["path"], "offset": off, "length": n, "source": "target"})
    nbytes = sum(u["length"] for u in units)
    if runs > MAX_RUNS or nbytes > MAX_SHARE * max(total, 1):
        return reload_plan(sp, "fragmented: %d changed runs, %d of %d bytes" % (runs, nbytes, total))
    for u in units:
        u["unit"] = "rows:%d-%d" % (u["offset"] // ROW, (u["offset"] + u["length"] - 1) // ROW)
    return {"schema": "onggi.updateplan.v1", "base_manifest": sp["base_manifest"], "manifest": sp["manifest"],
            "mode": "units", "units": units, "bytes": nbytes}


def verification(op, mode, manifest, units, written):
    return {"schema": "onggi.verification.v1", "op": op, "mode": mode, "manifest": manifest, "ok": True,
            "units": units, "bytes_written": written}


def apply(up, target):
    if up.get("schema") != "onggi.updateplan.v1":
        fail("unknown update plan schema %r" % up.get("schema"))
    st = state()
    if up["mode"] == "units" and st["manifest"] != up["base_manifest"]:
        fail("base not resident; nothing changed")
    tfiles = files_of(target)
    # stage: read and hash everything to write before touching the device
    if up["mode"] == "reload":
        staged = [{"file": p, "offset": 0, "data": read(a, 0, os.path.getsize(a))} for p, a in sorted(tfiles.items())]
    else:
        staged = []
        for u in up["units"]:
            if u["file"] not in tfiles:
                fail("%s: not in the target; nothing changed" % u["file"])
            d = read(tfiles[u["file"]], u["offset"], u["length"])
            if len(d) != u["length"]:
                fail("%s: target shorter than the unit; nothing changed" % u["file"])
            staged.append({"file": u["file"], "offset": u["offset"], "data": d})
    # journal: what rollback needs (here: the whole resident state for a reload, the old rows otherwise)
    shutil.rmtree(JOURNAL, ignore_errors=True)
    os.makedirs(JOURNAL)
    if up["mode"] == "reload":
        if os.path.isdir(MEM):
            shutil.copytree(MEM, os.path.join(JOURNAL, "mem"))
    else:
        for i, s in enumerate(staged):
            with open(os.path.join(JOURNAL, "%d" % i), "wb") as f:
                f.write(read(os.path.join(MEM, s["file"]), s["offset"], len(s["data"])))
    with open(os.path.join(JOURNAL, "journal.json"), "w") as f:
        json.dump({"manifest": st["manifest"], "mode": up["mode"],
                   "units": [{"file": s["file"], "offset": s["offset"], "length": len(s["data"])} for s in staged]}, f)
    # apply (a real binding does this at a drained boundary), then read back and verify
    if up["mode"] == "reload":
        shutil.rmtree(MEM, ignore_errors=True)
    written, units = 0, []
    for s in staged:
        p = os.path.join(MEM, s["file"])
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "r+b" if os.path.exists(p) else "wb") as f:
            f.seek(s["offset"])
            f.write(s["data"])
        written += len(s["data"])
        got = read(p, s["offset"], len(s["data"]))
        if got != s["data"]:
            rollback()
            fail("read-back mismatch in %s; rolled back" % s["file"], 3)
        units.append({"file": s["file"], "offset": s["offset"], "length": len(got), "sha256": sha(got)})
    save_state({"manifest": up["manifest"]})
    out(verification("apply", up["mode"], up["manifest"], units, written))


def rollback(emit=False):
    try:
        with open(os.path.join(JOURNAL, "journal.json")) as f:
            j = json.load(f)
    except (OSError, ValueError):
        fail("nothing to roll back")
    units, written = [], 0
    if j["mode"] == "reload":
        shutil.rmtree(MEM, ignore_errors=True)
        if os.path.isdir(os.path.join(JOURNAL, "mem")):
            shutil.copytree(os.path.join(JOURNAL, "mem"), MEM)
        for p, a in sorted(files_of(MEM).items()):
            d = read(a, 0, os.path.getsize(a))
            written += len(d)
            units.append({"file": p, "offset": 0, "length": len(d), "sha256": sha(d)})
    else:
        for i, u in enumerate(j["units"]):
            with open(os.path.join(JOURNAL, "%d" % i), "rb") as f:
                old = f.read()
            p = os.path.join(MEM, u["file"])
            with open(p, "r+b") as f:
                f.seek(u["offset"])
                f.write(old)
            written += len(old)
            units.append({"file": u["file"], "offset": u["offset"], "length": len(old),
                          "sha256": sha(read(p, u["offset"], len(old)))})
    save_state({"manifest": j["manifest"]})
    shutil.rmtree(JOURNAL, ignore_errors=True)
    if emit:
        out(verification("rollback", j["mode"], j["manifest"], units, written))


def load(artifact, manifest):
    shutil.rmtree(DEV, ignore_errors=True)
    shutil.copytree(artifact, MEM)
    save_state({"manifest": manifest})


def main(argv):
    if not argv:
        fail(__doc__, 64)
    cmd, args = argv[0], argv[1:]
    if cmd == "describe" and len(args) == 1:
        describe(args[0])
    elif cmd == "resident" and not args:
        resident()
    elif cmd == "plan" and not args:
        out(plan(json.load(sys.stdin)))
    elif cmd == "apply" and len(args) == 2 and args[0] == "--target":
        apply(json.load(sys.stdin), args[1])
    elif cmd == "rollback" and not args:
        rollback(emit=True)
    elif cmd == "conformance-load" and len(args) == 2:
        load(args[0], args[1])
    else:
        fail("usage: see the module docstring", 64)


if __name__ == "__main__":
    main(sys.argv[1:])
