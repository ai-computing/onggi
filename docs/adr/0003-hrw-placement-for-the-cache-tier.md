# ADR-0003: Rendezvous (HRW) hashing places chunks in the cache tier

Status: proposed (2026-10-21)

## Context
Nodes need to find which peers should hold a chunk without a central index (a chunk index for hundreds of GB is too
large for etcd and a single point of failure).

## Decision
Owners of a chunk are the top-R live agents by `score = hash(chunk, node)`, filtered so replicas span topology labels.
Membership comes from agents' Lease objects. Agents also announce chunks they hold beyond ownership (hot chunks,
recent reads) so peers can fetch from them.

## Consequences
Any node computes owners locally; a membership change moves ≈ 1/N of owned chunks. Hot chunks need extra replicas on
top of ownership (by popularity). Anti-entropy repairs owners that miss chunks.
