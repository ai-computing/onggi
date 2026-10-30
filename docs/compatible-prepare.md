# Select compatible artifacts and prepare files

[한국어](ko/compatible-prepare.md)

This is the first P3 integration increment. `onggi prepare` selects among
explicit candidates and prepares verified files before an engine starts.
It works with the existing POSIX/S3 stores, node chunk cache and peers. It does
not compile, discover installed SDKs, infer ABI compatibility, allocate an NPU,
warm a device graph or consume placement/autoscaler events.

## Publish an explicit profile

Publish each supported batch/sequence profile with its compiled directory:

```sh
onggi push --store "$STORE" --profile rngd-b4-s256.json --ref llama/rngd/b4-s256 ./build
```

Example profile structure (replace the digests and build identities with actual
values; these illustrative identities are not a hardware compatibility claim):

```json
{
  "schema": "onggi.profile.v1",
  "model_source": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
  "options": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
  "device": "rngd",
  "runtime": "rngd-pd@<immutable-runtime-commit>",
  "sdk": "2026.4.0",
  "compiler": "<exact-compiler-build>",
  "firmware": "<exact-firmware-build>",
  "format": "runnable",
  "layout": "<exact-bf16-layout-id>",
  "topology": "fusion8-mesh2-group2",
  "tp": 1,
  "batch": 4,
  "sequence": 256
}
```

`model_source` identifies an immutable checkpoint file/hash manifest, not a
mutable model name. `options` is the SHA256 of the exact build-options record;
publisher and consumer must use the same canonical file bytes. Include all
relevant options (precision, compiler flags, parallelism, graph grouping, etc.).
Runtime, firmware, layout and topology strings must identify exact builds and
arrangements. An ATOM artifact uses its actual device SKU, rebel/compiler and
firmware identities, immutable runtime build, `rbln` format, actual weight
layout and P/N topology; it does not reuse an RNGD profile or SDK version.
The selector treats identities as opaque exact strings and knows no vendor ABI.

The manifest's content hash binds this declaration to its file/chunk lists.
Accuracy remains the publisher's responsibility; this is not a signed vendor
attestation. Existing builds without a profile still support ordinary
`push`/`pull`; automatic selection excludes them. No storage-format migration
or duplicate weight bytes are required to add a profiled ref.

## Select, then prepare

Create `request.json` with the complete desired profile, in the same schema:

```sh
onggi prepare --store "$STORE" --request request.json --plan-only \
  llama/rngd/b1-s256 llama/rngd/b4-s256

onggi prepare --store "$STORE" --request request.json \
  --cache /var/lib/onggi/cache --dir /models/ready \
  llama/rngd/b1-s256 llama/rngd/b4-s256
```

Every field must match, including exact batch/sequence: larger graphs are not
assumed to support padding. Unknown JSON fields, missing fields, blank/unknown
identities, noncanonical/zero digests and nonpositive dimensions are rejected.
All readable candidates are reported; missing/corrupt manifests fail closed.
For multiple matches, the lexically lowest manifest digest wins, independently
of argument order. Refs resolve once under a GC lease; subsequent ref movement
does not change the selected artifact. For reproducible jobs, supply digests.

Stdout is a JSON report: `selection` contains the chosen manifest, compatible
digests and rejection reasons; `ready` is true only after verified publication;
`directory` and `stats` describe the published files and cache/store traffic.
Exit status 0 means selection/preparation succeeded; 1 means an operational
failure/no match; 2 means invalid CLI/profile input. Plan-only succeeds with
`ready:false` and does not create destination/cache or fetch model chunks.
Opening a backend follows the existing CLI behavior and can initialize an
empty store; plan-only is not a globally read-only backend operation.

The destination must be absent. Preparation uses a sibling private temp tree,
hash-checks through the existing pull path, and renames only the completed tree.
A sibling `<destination>.onggi-lock` directory excludes cooperating preparers.
Ordinary failures/cancellation remove this call's temp/lock and preserve useful
cache chunks. Existing destinations, symlinks and another preparer's lock are
refused. A process crash can leave temp/lock files; verify the owner is gone
before removing them or choose a fresh destination. There is no automatic
adoption of an existing directory or crash-durable publication guarantee.
File modes remain those of the artifact; arrange engine UID/group accordingly.
Do not share this preparation root with the CSI materializer's managed root.

The [init-container example](../deploy/k8s/prepare-initcontainer.yaml) gates
engine startup on successful file preparation. The engine still validates its
native artifacts, loads/prepares graphs and reports serving readiness. The
RNGD and ATOM resident-update paths remain separate runtime integrations;
this command does not implement live request draining, variant switching or
device mutation. Cluster-wide discovery, CRDs/compile farm, placement-driven
prefetch and native prewarming remain later P3/P4 work.
