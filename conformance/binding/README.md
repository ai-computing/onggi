# Device binding conformance kit

Checks a device binding against the interface of [ADR-0010](../../docs/adr/0010-device-binding-contract.md).
Python 3.6+, standard library only, Apache-2.0.

```bash
python3 conformance.py                          # the reference fake binding, level B2
python3 conformance.py --level B1               # the fake binding acting as B1 (plan always reloads)
python3 conformance.py --level B2 -- ./my-binding --device 0   # your binding
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
