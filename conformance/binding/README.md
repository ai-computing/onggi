# Device binding conformance kit

Checks a device binding against the interface of [ADR-0010](../../docs/adr/0010-device-binding-contract.md).
Python 3.6+, standard library only, Apache-2.0.

```bash
python3 conformance.py                          # the reference fake binding, level B2
python3 conformance.py --level B1               # the fake binding acting as B1 (plan always reloads)
python3 conformance.py --level B2 -- ./my-binding --device 0   # your binding
python3 conformance.py --level B2 --fixtures /runs/fixtures.json --report /runs/result.json -- ./my-binding
```

Your binding is run as `COMMAND <operation> [args]` with JSON on stdin and stdout and a non-zero exit on error:

| operation | stdin | stdout |
|---|---|---|
| `describe ARTIFACT_DIR` | — | `onggi.profile.v1` |
| `resident` | — | `onggi.resident.v1` |
| `plan` | `onggi.switchplan.v1` | `onggi.updateplan.v1` |
| `apply --target DIR` | `onggi.updateplan.v1` | `onggi.verification.v1` |
| `rollback` | — | `onggi.verification.v1` |
| `conformance-load ARTIFACT_DIR MANIFEST` | — | — (kit hook: load the base artifact the normal way and remember MANIFEST as its identity) |

Schemas and examples: [`docs/schemas/`](../../docs/schemas/).

## Runtime artifact fixtures

`--fixtures FILE` selects external artifact directories instead of the four synthetic switches. Options for the
kit go before `--`; the command and options for your binding go after it. A provider file has this shape:

```json
{
  "schema": "onggi.binding-fixtures.v1",
  "cases": [
    {
      "name": "native-identical",
      "base_dir": "/runs/artifacts/base",
      "target_dir": "/runs/artifacts/base",
      "switchplan": "identical.switchplan.json",
      "expect_mode": "units"
    },
    {
      "name": "native-inplace",
      "base_dir": "/runs/artifacts/base",
      "target_dir": "/runs/artifacts/target",
      "switchplan": "inplace.switchplan.json",
      "expect_mode": "units"
    }
  ]
}
```

`cases` must be nonempty, with unique nonempty names. Every case requires all five fields shown. Relative
`base_dir`, `target_dir`, and `switchplan` paths resolve against the provider file's directory. `switchplan` may
instead contain an inline `onggi.switchplan.v1` object. Obtain it with `onggi diff --ranges --json BASE TARGET`;
the provider is responsible for supplying verified artifacts and their authentic manifest identities. The kit
checks manifest syntax, the complete target file inventory and sizes, paired/source base files, range partitions,
and byte counts before invoking the binding. Every `same` range must equal the paired base at the same offset;
every `elsewhere` range must equal its declared base source interval. Artifact directories must exist and contain regular files; symlinks
are rejected so no files are silently skipped or read outside those directories.

`expect_mode` is exactly `units` or `reload`. At B2 each case must return its declared mode, and at least one case
must declare `units` with changed ranges (`switchplan.moved + switchplan.new > 0`) and an actual byte difference
between target files and the same-path base files (or paired base files when the path is new). A nonempty target
file without a base counterpart also counts. Reload-only or identical-only fixtures cannot establish B2, even
if unchanged bytes are labeled `new`. Individual `new` spans may include equal bytes, as chunk boundaries can
produce such spans. At B1 every case must declare `reload`, and the binding must always return it.
B2 retains coverage, apply readback, resident identity,
rollback restoration, and second-rollback refusal checks for these fixtures. A runtime supporting a bounded
lossless subset can provide native identical and in-place cases with `units`; a passing fixture run establishes
that declared subset, rather than support for every native switch. Default runs still require `units` for the
synthetic `identical` and `inplace` cases and still run all four synthetic cases.

The checker uses file sizes from filesystem metadata and compares or hashes intervals with reads of at most 1 MiB.
It neither copies external artifacts nor reads a whole model into memory. Keep the directories unchanged while
the kit runs. Binding-side staging and memory use remain the binding's responsibility.

`--report FILE` exclusively creates a new JSON file (`onggi.binding-report.v1`); an existing file is never
overwritten. Reports include the level, binding command, provider file SHA-256, and, for each executed case,
the resolved artifact directories, expected mode, artifact file sizes and SHA-256 hashes, and a SHA-256 of the
switchplan's canonical JSON (sorted keys, compact separators, UTF-8). Each operation records its full command,
JSON input, exit code and JSON output, or its execution error. The final `ok`, `failures`, and optional `error`
also record unsuccessful runs. Hashing the complete artifacts for a report is streamed. Without `--fixtures`,
reports describe the synthetic cases and omit the provider file field. Reports contain paths, commands, and
binding output; choose the destination accordingly.

## What is in here

- `golden/*.switchplan.json`: what `onggi diff --ranges --json` prints for four synthetic switches of a 2 MiB
  `model.bin` and a `config.json`: `identical`, `inplace` (40 KiB overwritten, config changed), `shifted` (17,952 B
  inserted in front, as a larger TensorRT-LLM batch does) and `rearranged` (the same bytes in 32 KiB blocks in another
  order). `go test ./pkg/store -run GoldenSwitchPlans` fails when onggi's output drifts from them (`-update`
  rewrites them).
- `golden_data.py`: generates the bytes of those artifacts (SHA-256 in counter mode, the same in Go).
- `fake_binding.py`: the reference binding on a simulated device (a directory of 4 KiB rows). It maps changed runs to
  rows, answers `reload` when the base is not resident or more than 64 runs or 25 % of the bytes change, stages and
  journals before writing, reads back to verify, and rolls back from the journal.
- `conformance.py`: the tests, per golden switch:
  1. `describe` returns `onggi.profile.v1`; after `conformance-load`, `resident` names the base.
  2. `plan` is pure: the same answer twice and the resident state unchanged.
  3. The update plan names the switch plan's base and target; at B1 it is always `reload`; at B2 the `identical` and
     `inplace` switches are applied as units, and units cover every byte that is not `same`.
  4. A plan for a base that is not resident is refused (`reload` or an error).
  5. B2: `apply` returns digests equal to the target's bytes (all target files for a reload), the target is then
     resident, `rollback` returns digests equal to the base's bytes, the base is resident again, and a second
     rollback fails.

A real binding computes the digests from what it reads back from the device, mapped to artifact byte order. The kit
does not check timing or draining; those belong to the runtime's own tests.
