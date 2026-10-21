# ADR-0006: Go for cluster components, Python for vendor adapters

Status: proposed (2026-10-21)

## Context
The controller, agent, CSI plugin and chunker fit the Kubernetes Go ecosystem (controller-runtime, CSI libraries,
the llm-d Go template). Vendor toolchains (furiosa-llm, optimum-rbln) are Python.

## Decision
Go for everything that runs as a cluster service and for the CLI. Python adapters run inside compile Jobs, read and
write vendor artifacts, and talk to onggi through the CLI or the HTTP API.

## Consequences
Two languages, with a clear seam (adapters produce a directory + layout tags; the core does the rest).
