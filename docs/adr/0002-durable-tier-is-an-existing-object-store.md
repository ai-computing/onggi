# ADR-0002: The durable tier is an existing object store

Status: proposed (2026-10-21)

## Context
Durable, multi-node, erasure-coded storage is hard to get right and is not what makes onggi different. Proven systems
exist (MinIO distributed, Ceph RGW, cloud object stores).

## Decision
Onggi talks to the durable tier through an S3-compatible (and OCI-registry) backend interface and never implements
its own durability. Chunks are packed into objects; manifests are objects addressed by digest. A local-filesystem
backend exists for tests and single-node labs.

## Consequences
Durability, rebuild and erasure coding come from the store. Onggi's own correctness surface is the cache tier, the
manifests and the refs. The lab must run a durable store (open question: MinIO vs Ceph vs a gateway in front of the
current SMB share).
