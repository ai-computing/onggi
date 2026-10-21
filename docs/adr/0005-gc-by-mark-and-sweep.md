# ADR-0005: GC by mark-and-sweep with leases and a grace period

Status: proposed (2026-10-21)

## Context
Reference counting on every publish and delete is error-prone under crashes and concurrent writers.

## Decision
Roots: refs, pins in StorePolicy, and leases held by in-progress publishes and mounted volumes. Mark everything
reachable from roots; sweep unmarked chunks older than a grace period. The durable tier and each cache sweep
independently. GC pauses while lease state is unknown (partition).

## Consequences
Publishing never coordinates with deletion. Space is reclaimed with a delay (grace period). Same approach as
container registries, git and restic.
