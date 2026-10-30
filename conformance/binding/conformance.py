#!/usr/bin/env python3
# Copyright 2026 The onggi authors. SPDX-License-Identifier: Apache-2.0
"""Conformance tests for an onggi device binding (ADR-0010). Python 3.6+, standard library only.

    python3 conformance.py [--level B1|B2] [-- BINDING COMMAND...]

Without a command it tests the reference fake binding. The binding command is run as
`COMMAND <operation> [args]` with JSON on stdin and stdout; see README.md.
"""
import hashlib
import json
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import golden_data  # noqa: E402

FAILS = []


def check(cond, what):
    print(("ok    " if cond else "FAIL  ") + what)
    if not cond:
        FAILS.append(what)
    return cond


def run(cmd, op, *args, stdin=None, expect_ok=True):
    p = subprocess.run(cmd + [op] + list(args), input=None if stdin is None else json.dumps(stdin).encode(),
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if expect_ok and p.returncode != 0:
        raise RuntimeError("%s %s: exit %d: %s" % (op, " ".join(args), p.returncode, p.stderr.decode()[-500:]))
    try:
        return p.returncode, json.loads(p.stdout.decode()) if p.stdout.strip() else None
    except ValueError:
        raise RuntimeError("%s: stdout is not JSON: %r" % (op, p.stdout[:200]))


def sha(b):
    return hashlib.sha256(b).hexdigest()


def covers(update, switch):
    """Every byte that is not `same` in the switch plan lies in a unit of the same file."""
    units = {}
    for u in update["units"]:
        units.setdefault(u["file"], []).append((u["offset"], u["offset"] + u["length"]))
    for f in switch["files"]:
        for r in f["ranges"]:
            if r["kind"] == "same":
                continue
            pos, end = r["offset"], r["offset"] + r["length"]
            for a, b in sorted(units.get(f["path"], [])):
                if a <= pos < b:
                    pos = b
            if pos < end:
                return False
    return True


def verify_units(ver, files, what):
    ok = ver.get("schema") == "onggi.verification.v1" and ver.get("ok") is True
    for u in ver.get("units", []):
        want = files.get(u["file"], b"")[u["offset"]:u["offset"] + u["length"]]
        ok = ok and len(want) == u["length"] and u["sha256"] == sha(want)
    return check(ok, what)


def main(argv):
    level, cmd = "B2", None
    if "--" in argv:
        cmd = argv[argv.index("--") + 1:]
        argv = argv[:argv.index("--")]
    if "--level" in argv:
        level = argv[argv.index("--level") + 1]
    work = tempfile.mkdtemp(prefix="onggi-binding-")
    if not cmd:
        os.environ["ONGGI_FAKE_DEVICE"] = os.path.join(work, "device")
        if level == "B1":
            os.environ["ONGGI_FAKE_LEVEL"] = "B1"
        cmd = [sys.executable, os.path.join(HERE, "fake_binding.py")]
    for name in golden_data.CASES:
        base, target = golden_data.case(name)
        bdir, tdir = os.path.join(work, name, "base"), os.path.join(work, name, "target")
        golden_data.write(bdir, base)
        golden_data.write(tdir, target)
        with open(os.path.join(HERE, "golden", name + ".switchplan.json")) as f:
            sp = json.load(f)
        print("== %s (%s)" % (name, level))

        _, prof = run(cmd, "describe", tdir)
        check(prof.get("schema") == "onggi.profile.v1", "describe returns onggi.profile.v1")

        run(cmd, "conformance-load", bdir, sp["base_manifest"])
        _, res = run(cmd, "resident")
        check(res.get("schema") == "onggi.resident.v1" and res.get("manifest") == sp["base_manifest"],
              "resident names the loaded base")

        _, up1 = run(cmd, "plan", stdin=sp)
        _, up2 = run(cmd, "plan", stdin=sp)
        _, res2 = run(cmd, "resident")
        check(up1 == up2 and res2 == res, "plan is pure: same answer twice, resident state unchanged")
        check(up1.get("schema") == "onggi.updateplan.v1" and up1.get("mode") in ("units", "reload"),
              "plan returns onggi.updateplan.v1 with mode units or reload")
        check(up1["base_manifest"] == sp["base_manifest"] and up1["manifest"] == sp["manifest"],
              "update plan names the switch plan's base and target")
        if level == "B1":
            check(up1["mode"] == "reload", "B1: plan always answers reload")
        elif name in ("identical", "inplace"):
            check(up1["mode"] == "units", "B2: a switch that keeps 97 % in place is applied as units")
        if up1["mode"] == "units":
            check(covers(up1, sp), "units cover every byte that is not at the same offset")

        wrong = dict(sp, base_manifest="sha256:" + "0" * 64)
        rc, upw = run(cmd, "plan", stdin=wrong, expect_ok=False)
        check(rc != 0 or (upw or {}).get("mode") == "reload", "plan against a base that is not resident refuses (reload)")

        if level == "B1":
            continue
        _, ver = run(cmd, "apply", "--target", tdir, stdin=up1)
        verify_units(ver, target, "apply verifies: unit digests equal the target bytes")
        if up1["mode"] == "reload":
            check(sorted(u["file"] for u in ver["units"]) == sorted(target), "reload writes every target file")
        _, res3 = run(cmd, "resident")
        check(res3.get("manifest") == sp["manifest"], "after apply the target is resident")

        _, rb = run(cmd, "rollback")
        check(rb.get("op") == "rollback", "rollback returns a verification")
        verify_units(rb, base, "rollback restores: unit digests equal the base bytes")
        _, res4 = run(cmd, "resident")
        check(res4.get("manifest") == sp["base_manifest"], "after rollback the base is resident")
        rc, _ = run(cmd, "rollback", expect_ok=False)
        check(rc != 0, "a second rollback has nothing to undo and fails")

    print("\n%d failed" % len(FAILS))
    return 1 if FAILS else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
