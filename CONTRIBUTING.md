# Contributing

This repository follows the organization's workflow: <https://github.com/ai-computing/.github/blob/main/CONTRIBUTING.md>
(Issue first, a branch per purpose, a PR with evidence, all checks green, squash merge).

Additions for onggi:

- Design changes go through an ADR in `docs/adr/` (a new ADR supersedes an old one; accepted ADRs are not edited).
- Performance claims need the measurement method, hardware and baseline in the PR, as in `docs/artifact-formats.md`.
- No internal hostnames, addresses or credentials in tracked files; node names use aliases (`npu-node-1`).
