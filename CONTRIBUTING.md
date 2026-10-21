# Contributing

This repository follows the organization's workflow: <https://github.com/ai-computing/.github/blob/main/CONTRIBUTING.md>
(Issue first, a branch per purpose, a PR with evidence, all checks green, squash merge).

Additions for onggi:

- Design changes go through an ADR in `docs/adr/` (a new ADR supersedes an old one; accepted ADRs are not edited).
- Performance claims need the measurement method, hardware and baseline in the PR, as in `docs/artifact-formats.md`.
- No internal hostnames, addresses or credentials in tracked files; node names use aliases (`npu-node-1`).
- Code from elsewhere follows [ADR-0009](docs/adr/0009-clean-room-and-vendor-formats.md): Apache-2.0/MIT/BSD with
  notices and the source named in the PR; GPL, LGPL, AGPL and non-commercial code only clean-room, from the paper or
  specification. Cite the papers an implementation follows.
- Vendor artifacts (`.rbln`, `.edf`, bundles) are opaque bytes plus documented metadata. Do not decode undocumented
  vendor formats or infer compiler internals.
- Changes that depart from a published algorithm (chunking shortcuts, SIMD boundary search, lossy coding) need a note in
  the PR so maintainers can arrange an IP review before merge. Do not discuss specific patents in issues or PRs.
- Contributions are licensed under Apache-2.0 (§5), including its patent licence.
