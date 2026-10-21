# ADR-0007: Only refs live in the Kubernetes API; manifests are content-addressed objects

Status: proposed (2026-10-21)

## Context
etcd is strongly consistent but small. Manifests for large artifacts list many chunks.

## Decision
CRDs hold refs (name → manifest digest), sets, policies and per-node summaries. Manifests and chunk lists are objects
in the data plane, addressed by digest. A publish commits by a compare-and-swap on the Ref's resourceVersion.

## Consequences
etcd stays small; the commit point is a single atomic update; everything else is immutable and cacheable.
