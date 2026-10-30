# ADR-0010: Device binding interface

Status: proposed · 2026-10-30 (#75)

## Context
onggi stores, ships and switches compiled model variants as bytes. Only two parts of the system depend on the
accelerator vendor: **which variant an artifact is** (its coordinate: toolchain, device, topology, shape), which the
publisher knows, and **where an artifact's bytes live in device memory**, which only the runtime knows. Everything
between them (chunking, dedup, transfer, the three-way diff, in-place patching of files on a node) works without
reading any vendor format (ADR-0009).

A switch between variants changes few bytes (docs/evaluation.md: decode B2 → B8 keeps 98.6 % at the same offset; RNGD
pfbatch → preset 99.77 %), and `onggi diff --ranges --json` already lists them (#46). Runtime experiments (the rngd-pd
resident delta) consumed that output, but it had no schema or version, did not say which runs the base holds elsewhere
or where, and nothing stated what a runtime may assume or must guarantee. For onggi to work with any accelerator, the
runtime side needs one public, versioned interface that a vendor or runtime team can implement alone.

## Decision

### 1. A binding implements five operations
A **device binding** is a program or library on the serving side that implements the operations below. They are
defined as JSON in, JSON out, so a binding can be a separate process in any language (the reference is an executable:
`BINDING <operation> [args]`, input on stdin, output on stdout, non-zero exit on error with the device unchanged). No
Go type crosses the boundary.

| operation | input | output | side effects |
|---|---|---|---|
| `describe(artifact)` | an artifact directory | the coordinate, `onggi.profile.v1` (#71/#72) | none; runs on the publisher or compiler side, reads only documented metadata |
| `resident()` | — | `onggi.resident.v1`: manifest digest of the artifact resident on the device, and its state | none |
| `plan(switch_plan)` | `onggi.switchplan.v1` | `onggi.updateplan.v1`: device units to rewrite (`mode: units`) or `mode: reload` with a reason | **none** (pure); may always answer reload |
| `apply(update_plan, target)` | `onggi.updateplan.v1` and the verified target artifact directory | `onggi.verification.v1`: per unit, SHA-256 of what the device holds, in artifact byte space | stages, applies at a drained boundary, verifies |
| `rollback()` | — | `onggi.verification.v1` of the restored units | restores the state before the last apply |

### 2. Schemas
Checked in under [`docs/schemas/`](../schemas/) as JSON Schema (2020-12), with examples:

- [`switchplan.v1.schema.json`](../schemas/switchplan.v1.schema.json): exactly what `onggi diff --ranges --json BASE
  TARGET` prints. `schema: "onggi.switchplan.v1"`, `base_manifest`, `manifest`, and per file `ranges[]` =
  `{offset, length, kind: same|elsewhere|new, source_file, source_offset}` covering the file in order without gaps
  (`source_*` only for `elsewhere`). Every field of the earlier output (`same`, `moved`, `new`, `new_stored`,
  `same_blocks`, `changed_runs`, `largest_run`, per-file `changed` `[offset, length]` pairs) keeps its meaning, so
  existing consumers keep working.
- [`updateplan.v1.schema.json`](../schemas/updateplan.v1.schema.json): `mode`, `reason`, `bytes`, `units[]` =
  `{file, offset, length, unit, source}`, in artifact byte space, rounded out to whole device units (rows, tensors,
  pages: the binding's choice; `unit` is its own opaque name).
- [`verification.v1.schema.json`](../schemas/verification.v1.schema.json): `op`, `mode`, `manifest` now resident,
  `ok`, `bytes_written`, `units[]` = `{file, offset, length, sha256}`.
- [`resident.v1.schema.json`](../schemas/resident.v1.schema.json): `manifest` (or null), `state`
  (`empty`/`ready`/`staged`).
- `onggi.profile.v1` is the publisher-declared coordinate of #71/#72 (`store.Profile`); this ADR uses it unchanged.

Within v1, fields may only be added; consumers ignore fields they do not know. An incompatible change is a v2, and a
binding rejects a schema it does not know.

### 3. What onggi guarantees
- The switch plan comes from the two artifacts' content-addressed chunk lists: a `same` or `elsewhere` run is
  byte-identical to its source in the base artifact (SHA-256 of the chunks). It is exact at chunk granularity: bytes
  that chunk differently are reported as `elsewhere` or `new` even if some equal the base.
- The target's bytes are available as verified files (an onggi CSI volume or `onggi pull`).
- onggi never parses a vendor format or device layout, and never calls a vendor API.

### 4. What a binding must do
- **Plan purely.** `plan` reads the switch plan and the binding's own knowledge of the resident state and changes
  nothing. Given the same inputs it gives the same answer.
- **Map or refuse.** Every byte that is not `same` lies in some unit of the update plan, or the plan says `reload`. A
  binding answers `reload` when the resident artifact is not `base_manifest`, when a run cannot be mapped, or when the
  plan is too fragmented to be worth it (its own threshold). It never leaves a byte unmapped.
- **Read verified bytes.** `apply` reads target bytes from the verified target directory, not from the plan.
- **Stage, drain, verify.** Read and stage all units first, apply at a drained boundary (no request in flight on the
  affected model), read back and return a digest per unit. `ok: true` only if every unit verified.
- **Roll back.** Keep what restores the base state of every written unit until the caller accepts the verification;
  on any failure restore the base or unload the model. A failed update never leaves a mix of base and target serving.
- **Use public APIs only.** A binding may use only the vendor's public runtime APIs and documented metadata. It does
  not decode undocumented formats; onggi keeps its side of ADR-0009 by passing byte ranges only.
- **Report** the path taken (`units` or `reload`) and the bytes written, so each path's cost is measured.

### 5. Conformance levels

| level | binding | what onggi does |
|---|---|---|
| **B0** | none: the runtime reads files | store less (chunk dedup across variants and checkpoints), node delta (only missing chunks cross the network; in-place patching of a base copy on the node), CSI volumes. A switch restarts or reloads the engine from files. |
| **B1** | `describe`, `resident`, and `plan` that always answers `reload` | B0, plus selection of the compatible artifact by coordinate, and choosing the resident artifact as the base of the node switch (the files the runtime will reload are patched, not refetched). Graphs or tensors the runtime keeps across a reload stay its business. |
| **B2** | all five | B1, plus resident updates: only the device units an update plan names are rewritten, verified and roll-backable; `reload` remains the fallback for fragmented switches. |

A runtime starts at B0 and adds a binding when it wants device-level switches; each level is useful on its own.

### 6. Conformance kit
[`conformance/binding/`](../../conformance/binding/) holds what a vendor runs against its binding: golden switch plans
for four small synthetic switches (identical, in place, shifted by 17,952 B, rearranged in 32 KiB blocks) that a Go
test keeps equal to what `onggi diff --ranges --json` produces; a generator for their bytes; a reference fake binding
(Python, standard library, Apache-2.0) on a simulated device of 4 KiB rows; and `conformance.py`, which checks plan
purity, the refusal path (a base that is not resident), coverage of every changed byte, apply with digests equal to
the target, the resident identity after apply, rollback to digests equal to the base, and that B1 always reloads.
Bindings live with their runtimes (for example rngd-pd), not in onggi.

## Consequences
- A new accelerator joins by implementing a binding against public schemas and passing the kit; onggi needs no
  change and learns no format.
- Rearranged switches (decode → prefill: 0 % at the same offset) produce many `elsewhere` runs; bindings will usually
  answer `reload`, which is the intended outcome. The switch plan grows with the number of runs and is printed only
  with `--ranges`.
- File pairing in the switch plan is a heuristic (same path, else the closest size, preferring the same file kind by
  name); a binding that needs other pairs compares the files with the same rules.
- onggi does not yet invoke bindings itself; a node agent that calls `resident` to choose the base and `plan`/`apply`
  after a node switch is follow-up work.
