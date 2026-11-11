#!/usr/bin/env python3
"""Interleave two Onggi builds on one immutable store/ref pair; require equal JSON."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import statistics
import subprocess
import time


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("baseline")
    p.add_argument("candidate")
    p.add_argument("store")
    p.add_argument("base_ref")
    p.add_argument("new_ref")
    a = p.parse_args()
    env = dict(os.environ, GOMAXPROCS="4")
    samples, expected = [], None
    for i in range(6):
        order = ("baseline", "candidate") if i % 2 == 0 else ("candidate", "baseline")
        for name in order:
            t = time.perf_counter()
            result = subprocess.run(
                [getattr(a, name), "diff", "--store", a.store, "--ranges", "--json",
                 a.base_ref, a.new_ref], env=env, capture_output=True, check=True,
                timeout=120)
            elapsed = time.perf_counter() - t
            data = json.loads(result.stdout)
            if expected is None:
                expected = data
            if data != expected:
                raise RuntimeError("diff JSON changed between builds or repetitions")
            samples.append(dict(round=i, build=name, seconds=elapsed,
                                stdout_sha256=hashlib.sha256(result.stdout).hexdigest()))
    print(json.dumps(dict(
        schema=1, scope="CLI wall time including process start; no NPU execution",
        gomaxprocs=4, refs=[a.base_ref, a.new_ref],
        binary_sha256={n: hashlib.sha256(Path(getattr(a, n)).read_bytes()).hexdigest()
                       for n in ("baseline", "candidate")},
        medians={n: statistics.median(x["seconds"] for x in samples if x["build"] == n)
                 for n in ("baseline", "candidate")},
        exact_diff_json=True, samples=samples, diff=expected), indent=2))


if __name__ == "__main__":
    main()
