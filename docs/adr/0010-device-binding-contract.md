# ADR-0010: The range plan is the only interface to a runtime's device binding

Status: proposed · 2026-10-30 (#75)

## Context
A switch between two compiled variants of a model changes few bytes (docs/evaluation.md: decode B2 → B8 keeps 98.6 % at
the same offset; RNGD pfbatch → preset 99.77 %). onggi already finds those bytes by comparing two artifacts' chunk
lists, without reading any format (ADR-0009), and `onggi diff --ranges --json` has been what runtime experiments read
(#46; the rngd-pd resident delta). But that output had no schema name or version, listed only the changed runs without
saying which of them the base holds elsewhere, and nothing stated what a runtime may assume or must do with it.

Two parts of the system are vendor-specific: which variant an artifact is (declared by its publisher) and where an
artifact's bytes live in device memory (known only to the runtime). Everything between them works on bytes. The second
part, the **device binding**, needs one explicit contract so that every runtime joins the same way and onggi never has
to learn a format.

## Decision
1. **The range plan is the interface.** The JSON that `onggi diff --ranges --json <base> <target>` prints (Go:
   `store.DiffStats` with `DiffOptions{Ranges: true}`) is the only thing onggi hands to a device binding. A binding
   reads no onggi internals (packs, indexes, locators), and onggi reads no runtime or vendor structure.
2. **Schema `onggi.rangeplan.v1`:**

   | field | meaning |
   |---|---|
   | `schema` | `"onggi.rangeplan.v1"`; a binding rejects a plan whose schema it does not know |
   | `base_manifest`, `manifest` | digests of the base artifact the plan applies to and of the target it produces |
   | `files[]` | one entry per file of the target |
   | `files[].path` | the target file, relative to the artifact root |
   | `files[].base` | the paired base file (same path, else the closest in size, preferring the same file kind); empty when there is none |
   | `files[].size` | target file size in bytes |
   | `files[].ranges[]` | runs that cover the file from offset 0 to `size`, in order, without gaps or overlaps |
   | `.offset`, `.length` | the run in the target file, in bytes |
   | `.kind` | `same`: the bytes are at the same offset in `base`. `elsewhere`: the base holds them at `source_file`, `source_offset`. `new`: the base does not hold them |
   | `.source_file`, `.source_offset` | only for `elsewhere`: a base file and offset holding the same bytes |

   The summary fields of the earlier output (`same`, `moved`, `new`, `same_blocks`, `changed_runs`, `largest_run`,
   `new_stored`, and `changed` as `[offset, length]` pairs of the runs that are not `same`) stay as they were, so
   existing consumers keep working. Adjacent runs of one kind are merged; `elsewhere` runs are merged only where their
   sources are contiguous too. The plan is exact at chunk granularity: bytes that chunk differently are `new` or
   `elsewhere` even if some of them equal the base. A v2 is needed only for an incompatible change; new optional
   fields may be added to v1.
3. **What onggi guarantees.** The ranges come from the two manifests' content-addressed chunk lists, so a `same` or
   `elsewhere` run is byte-identical to its source in the base artifact (by SHA-256 of the chunks). The target
   artifact's bytes are available from `onggi pull`, which verifies every chunk. onggi does not know device layouts.
4. **What a binding must do.**
   - **Map or refuse.** Map each run that is not `same` to the device memory it affects, rounded out to whole device
     units (tensor rows, pages, tiles: the runtime's choice). If the runtime cannot map a run, or the plan is too
     fragmented to be worth it (its own threshold), it requests a **full reload** of the target instead. A partial
     update that leaves any unmapped byte is not allowed.
   - **Check the base.** Apply a plan only to a device state loaded from `base_manifest`; otherwise reload.
   - **Verify.** Read the target bytes from a verified source (an onggi volume or `onggi pull`), not from the plan.
     After the update, verify what the runtime can (for example device checksums of the written units, or a known
     prompt's output) before serving from it.
   - **Roll back.** Keep what is needed to restore the base state of every unit it writes until verification passed,
     apply at a drained boundary (no request in flight on the affected model), and on any failure restore the base or
     fall back to a full reload. A failed update must leave the base model serving or the model unloaded, never a mix.
   - **Report** which path it took (partial update or full reload), the bytes written to the device and the time, so
     the cost of each path is measured.
5. **Scope.** Bindings live with their runtimes (for example rngd-pd), not in onggi. A binding is the only
   vendor-specific code path of a switch; the variant a runtime needs is chosen from publisher-declared metadata, not
   by reading the artifact.

## Consequences
- A new runtime joins by implementing one binding against this schema; onggi needs no change.
- Plans for rearranged files (for example decode → prefill, 0 % at the same offset) consist of many `elsewhere` runs;
  a binding will usually request a full reload for those, and that is the intended outcome.
- The plan is as large as the number of runs: small for switches that keep offsets, large for rearranged files. It is
  printed only with `--ranges`.
- Pairing base and target files is a heuristic (path, then size and file kind by name); a binding that knows better pairs may compare the
  files itself with the same rules.
