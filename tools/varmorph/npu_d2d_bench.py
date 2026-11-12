#!/usr/bin/env python3
"""NPU micro-benchmark (NPU-A or NPU-B): can a target's weights be rebuilt on the device from
resident weights?

A video-style descriptor turns a variant into copies of runs from a sibling. This measures what
such a decode costs on the card, against uploading the weights from the host. Cases:
  h2d_<MiB>      contiguous host -> device copy (the upload a restart pays)
  d2d_<MiB>      contiguous device -> device copy of one tensor into another
  d2d_calls      512 device copies of one 32 KiB run each, one call per run
  d2d_strided    one call copying every other 32 KiB run of a 64 MiB region (a regular re-tiling
                 as one descriptor)
  d2d_gather     one call gathering the 32 KiB runs of a 64 MiB region in a random order
                 (index_select)
  use_256        a device reduction over a 256 MiB tensor already on the device
  d2duse_256     a device copy into that tensor, then the same reduction (timed together): if the
                 copy left the data on the host side, the reduction pays the upload
  h2duse_256     a host upload into that tensor, then the reduction

Every case runs in its own process, so a device error in one case does not leave the card in a bad
state for the next. Each case syncs the card around the timed region and repeats REPS times after
a warm-up. Values are finite bf16 (from a normal distribution), and results are compared as int16
bit patterns: a fast path that changes the bytes is reported as wrong.
argv: OUT.json PLATFORM(rngd|rbln) [REPS, default 5]          (driver)
      --case NAME PLATFORM REPS OUT_CASE.json                 (one case, internal)
"""
import json
import os
import subprocess
import sys
import time
import traceback

RUN = 16384  # bf16 elements per 32 KiB run
CASES = ["h2d_64", "h2d_1024", "d2d_1", "d2d_64", "d2d_256", "d2d_1024",
         "d2d_calls", "d2d_strided", "d2d_gather",
         "use_256", "d2duse_256", "h2duse_256"]
if os.environ.get("RBLN_VMSHIM"):  # NPU-B batched copies through the public runtime API
    CASES += ["v2vmulti_1", "v2vmulti_64", "v2vmulti_1024", "v2vmultis_1024", "h2vmulti_1024"]


def device(platform):
    import torch
    if platform == "cpu":  # local test of the drivers
        return "cpu", (lambda: None), ("torch",)
    if platform == "rngd":
        from rngd_pd import devices
        from rngd_pd.engines.fsdk import dev_str, open_device, sync
        chip = devices.resolve_chip(-1)
        dev = dev_str(chip)
        open_device(8, 2)
        return dev, (lambda: sync(chip)), ("torch", "furiosa-torch")
    import rebel  # noqa: F401
    import torch_rbln  # noqa: F401  registers the rbln device
    dev = "rbln:%d" % int(os.environ.get("RBLN_BENCH_DEVICE", "0"))
    return dev, (lambda: torch.rbln.synchronize(dev)), ("torch", "rebel-compiler")


def run_case(name, platform, reps, out):
    import torch
    from importlib.metadata import version
    torch.set_num_threads(4)
    row = dict(case=name, s=[])
    try:
        dev, sync, pk = device(platform)
        row.update(device=dev, packages={k: version(k) for k in pk})
        g = torch.Generator().manual_seed(7)
        bits = lambda t: t.view(torch.int16)

        def host(nelem):
            return torch.randn(nelem, generator=g, dtype=torch.float32).to(torch.bfloat16)

        kind, _, arg = name.partition("_")
        if kind in ("v2vmulti", "v2vmultis", "h2vmulti"):
            import ctypes
            lib = ctypes.CDLL(os.environ["RBLN_VMSHIM"])
            k = int(arg)
            rows = 2048                                   # 64 MiB region of 32 KiB runs
            n = rows * RUN
            h = host(n)
            src = h.to(dev)
            dst = torch.empty(n, dtype=torch.bfloat16, device=dev)
            dst.copy_(src)                                # give dst a device view
            sync()
            if kind != "h2vmulti":
                row["layout_like_rc"] = lib.vmshim_layout_like(ctypes.c_uint64(dst.data_ptr()), ctypes.c_uint64(src.data_ptr()))
            perm = torch.randperm(rows, generator=g)[:k].tolist()
            U64 = ctypes.c_uint64 * k
            sz = U64(*([RUN * 2] * k))
            dsta = U64(*[dst.data_ptr() + i * RUN * 2 * (2 if kind == "v2vmultis" else 1) for i in range(k)])
            if kind == "h2vmulti":
                hb = h.view(rows, RUN)
                srca = U64(*[hb[r].data_ptr() for r in perm])
                fn = lambda: lib.vmshim_h2v_multi(srca, dsta, sz, k)
            else:
                srca = U64(*[src.data_ptr() + r * RUN * 2 for r in perm])
                fn = lambda: lib.vmshim_v2v_multi(srca, dsta, sz, k)
            nbytes = k * RUN * 2
            row["calls"] = 1
            step = 2 if kind == "v2vmultis" else 1
            check = lambda: all(torch.equal(bits(dst[(i * step) * RUN:(i * step + 1) * RUN].cpu()), bits(h[perm[i] * RUN:(perm[i] + 1) * RUN]))
                                for i in sorted({0, k // 2, k - 1}))
            rc = fn()
            row["rc"] = rc
        elif kind in ("use", "d2duse", "h2duse"):
            n = int(arg) * (1 << 20) // 2
            h = host(n)
            src = h.to(dev)
            dst = torch.empty(n, dtype=torch.bfloat16, device=dev)
            dst.copy_(src)
            sync()
            red = lambda: dst.sum()
            if kind == "use":
                fn = lambda: red()
            elif kind == "d2duse":
                fn = lambda: (dst.copy_(src), red())
            else:
                fn = lambda: (dst.copy_(h), red())
            nbytes = n * 2
            check = lambda: torch.equal(bits(dst[-RUN:].cpu()), bits(h[-RUN:]))
        elif kind == "h2d":
            n = int(arg) * (1 << 20) // 2
            h = host(n)
            dst = torch.empty(n, dtype=torch.bfloat16, device=dev)
            fn = lambda: dst.copy_(h)
            nbytes = n * 2
            check = lambda: torch.equal(bits(dst[-RUN:].cpu()), bits(h[-RUN:]))
        else:
            n = (int(arg) if arg.isdigit() else 64) * (1 << 20) // 2
            n = max(n // RUN, 2) * RUN
            rows = n // RUN
            h = host(n)
            src = h.to(dev)
            dst = torch.empty(n, dtype=torch.bfloat16, device=dev)
            sync()
            if arg.isdigit():
                fn = lambda: dst.copy_(src)
                nbytes = n * 2
                check = lambda: torch.equal(bits(dst[-RUN:].cpu()), bits(h[-RUN:]))
            elif arg == "calls":
                k = min(512, rows)
                perm = torch.randperm(rows, generator=g)[:k].tolist()

                def fn():
                    for i, r in enumerate(perm):
                        dst[i * RUN:(i + 1) * RUN].copy_(src[r * RUN:(r + 1) * RUN])
                nbytes = k * RUN * 2
                row["calls"] = k
                check = lambda: torch.equal(bits(dst[5 * RUN:6 * RUN].cpu()), bits(h[perm[5] * RUN:(perm[5] + 1) * RUN]))
            elif arg == "strided":
                s2, d2 = src.view(rows // 2, 2, RUN), dst.view(rows // 2, 2, RUN)
                fn = lambda: d2[:, 0, :].copy_(s2[:, 1, :])
                nbytes = (rows // 2) * RUN * 2
                check = lambda: torch.equal(bits(d2[:, 0, :].cpu()), bits(h.view(rows // 2, 2, RUN)[:, 1, :]))
            else:  # gather
                pidx = torch.randperm(rows, generator=g)
                pdev = pidx.to(dev)
                sv, dv = src.view(rows, RUN), dst.view(rows, RUN)
                fn = lambda: dv.copy_(torch.index_select(sv, 0, pdev))
                nbytes = n * 2
                check = lambda: torch.equal(bits(dv[7].cpu()), bits(h.view(rows, RUN)[pidx[7]]))
        row["bytes"] = nbytes
        fn()
        sync()
        for _ in range(reps):
            sync()
            t = time.perf_counter()
            fn()
            sync()
            row["s"].append(time.perf_counter() - t)
        row["median_s"] = sorted(row["s"])[len(row["s"]) // 2]
        row["GBps"] = nbytes / row["median_s"] / 1e9
        if "calls" in row:
            row["us_per_call"] = row["median_s"] / row["calls"] * 1e6
        row["correct"] = bool(check())
    except Exception as e:
        row["error"] = repr(e)[:400]
        row["trace"] = traceback.format_exc()[-1500:]
    with open(out, "w") as f:
        json.dump(row, f, indent=1)


def driver(out, platform, reps):
    res = dict(schema="npu-d2d.v2", platform=platform, reps=reps, load=os.getloadavg(), cases={})
    for name in CASES:
        part = out + "." + name + ".json"
        t = time.time()
        try:
            p = subprocess.run([sys.executable, __file__, "--case", name, platform, str(reps), part],
                               capture_output=True, text=True, timeout=600)
            row = json.load(open(part)) if os.path.exists(part) else dict(error="no output", rc=p.returncode)
            if p.returncode:
                row["rc"] = p.returncode
                row["stderr"] = p.stderr[-800:]
        except subprocess.TimeoutExpired:
            row = dict(error="timeout 600 s")
        row["wall_s"] = round(time.time() - t, 1)
        res["cases"][name] = row
        with open(out, "w") as f:
            json.dump(res, f, indent=1)
        print(name, {k: v for k, v in row.items() if k not in ("s", "trace", "stderr")}, flush=True)
    res["finished"] = time.time()
    with open(out, "w") as f:
        json.dump(res, f, indent=1)


if __name__ == "__main__":
    if sys.argv[1] == "--case":
        run_case(sys.argv[2], sys.argv[3], int(sys.argv[4]), sys.argv[5])
    else:
        driver(sys.argv[1], sys.argv[2], int(sys.argv[3]) if len(sys.argv) > 3 else 5)
