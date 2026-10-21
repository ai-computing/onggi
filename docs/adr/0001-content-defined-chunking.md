# ADR-0001: Content-defined chunking; chunk size chosen by measurement

Status: proposed (2026-10-21)

## Context
ATOM build variants share 99.3 % of their bytes, but at shifted offsets: fixed 1 MiB and 4 KiB blocks found 0 %
sharing, content-defined chunks with a 64 KiB average found the full share ([artifact-formats.md](../artifact-formats.md)).
Smaller chunks dedup better and cost a larger index and more objects.

## Decision
Use FastCDC-style content-defined chunking with BLAKE3 (or BLAKE2b) chunk hashes. Start at 64 KiB average
(16 KiB min, 256 KiB max). Pack chunks into larger durable objects. Before P1 ends, measure 64 KiB, 256 KiB, 1 MiB
and 4 MiB averages on the ATOM and RNGD sets (dedup ratio, index size, write and read throughput) and fix the default
in an ADR that supersedes this one.

## Consequences
Dedup works across shifted layouts. The chunker is the hot loop: it gets CI benchmarks and must run at disk speed.
Chunk size can differ per layout if the measurements say so; the manifest records the parameters.
