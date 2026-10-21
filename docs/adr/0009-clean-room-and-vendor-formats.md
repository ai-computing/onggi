# ADR-0009: Clean-room code and vendor formats

Status: accepted · 2026-10-21

## Context
Onggi stores artifacts produced by vendor compilers and builds on published techniques ([related work](../related-work.md)).
Three risks follow:

- **Code licences.** Some related projects are GPL, LGPL or non-commercial. Their code cannot enter an Apache-2.0
  repository.
- **Vendor licence terms.** The Rebellions Software User License Agreement
  ([public copy](https://github.com/rebellions-sw/rbln-npu-feature-discovery/blob/main/LICENSE)) forbids reverse
  engineering, decompiling and decoding (§3(f)) and competitive analysis (§3(l)). FuriosaAI's SDK terms are not public;
  we assume similar restrictions.
- **Patents.** Chunking, dedup and floating-point compression are crowded fields.

## Decision
1. **Vendor artifacts are opaque.** Onggi may hash, chunk, compress and move the bytes of an artifact the user compiled,
   and may read metadata the vendor documents or ships in plain form (`artifact.json`, `config.json`, safetensors
   headers, zip directories, file names). It does not parse or infer undocumented internals (graph encodings,
   weight-layout transforms) without the vendor's written permission. Format-aware features use only those inputs or
   statistics of the bytes themselves.
2. **Issue #10 (layout recovery with probe weights) is on hold** until a vendor grants permission or publishes the
   layout. Until then, nothing derived from it may land in this repository.
3. **Code reuse by licence:**

   | Licence of the source | Reuse |
   |---|---|
   | Apache-2.0, MIT, BSD | allowed; keep notices, record the source in the PR |
   | GPL, LGPL, AGPL, Artistic-2.0, non-commercial (CC BY-NC) | no code; implement from the paper or specification and say so in the PR |
   | papers | ideas allowed; cite them in code comments and docs |

4. **Published algorithms are implemented as published.** The FastCDC chunker follows the 2016/2020 papers. Departures
   from a published algorithm are reviewed with the institution's IP office before merge. This covers new chunking
   shortcuts, SIMD or multi-stage boundary search, and lossy weight coding. Patent analysis is not done in this public
   repository.
5. **Measurements, not vendor rankings.** Docs report each vendor's artifacts on their own terms. Cross-vendor
   performance comparisons are not published without checking the vendors' terms.
6. **Contributions** are accepted under Apache-2.0 §5, which includes the contributor's patent licence.

## Consequences
- ATOM files are stored at CDC dedup plus generic compression. The extra savings a layout transform might give are
  out of reach until permission exists.
- RNGD keeps its full benefit: its weight files equal the HF source, which a hash comparison shows.
- Reviewers check provenance and licence for any code taken from elsewhere.
