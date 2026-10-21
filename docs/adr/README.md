# Architecture decision records

One file per decision (Nygard format: context, decision, consequences). Status: proposed → accepted → superseded.
A decision changes only through a new ADR that supersedes the old one.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-content-defined-chunking.md) | Content-defined chunking; chunk size chosen by measurement | proposed |
| [0002](0002-durable-tier-is-an-existing-object-store.md) | The durable tier is an existing object store | superseded by 0008 |
| [0003](0003-hrw-placement-for-the-cache-tier.md) | Rendezvous (HRW) hashing places chunks in the cache tier | proposed |
| [0004](0004-delivery-through-csi.md) | Artifacts reach pods through a CSI volume | proposed |
| [0005](0005-gc-by-mark-and-sweep.md) | GC by mark-and-sweep with leases and a grace period | proposed |
| [0006](0006-go-core-python-adapters.md) | Go for cluster components, Python for vendor adapters | proposed |
| [0007](0007-refs-in-the-kubernetes-api.md) | Only refs live in the Kubernetes API; manifests are content-addressed objects | proposed |
| [0008](0008-pluggable-storage-backends.md) | Pluggable storage backends behind one interface; durability delegated to the backend | proposed |
| [0009](0009-clean-room-and-vendor-formats.md) | Vendor artifacts are opaque; clean-room code; published algorithms implemented as published | accepted |
