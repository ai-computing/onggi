# ADR-0004: Artifacts reach pods through a CSI volume

Status: proposed (2026-10-21)

## Context
Engines (vLLM, vllm-rbln, furiosa-llm, rngd-pd, Optimus Prime) read files from a path and must not need changes.
Kubernetes offers CSI drivers, image volumes (OCI artifacts) and init containers.

## Decision
Primary path: a CSI node plugin in onggi-agent (`onggi.ai-computing.github.io`, ephemeral inline volumes). `NodePublishVolume`
materializes the artifact as regular files on local disk (reflink/hardlink from the cache) and bind-mounts it
read-only; the pod stays Pending until it is ready. Fallbacks: Kubernetes image volumes where the cluster supports
them, and an init container. Lazy FUSE reads are an option for cold start only.

## Consequences
No engine changes; kubelet owns the mount lifecycle. Materializing whole files costs local disk equal to the
artifact's distinct bytes, shared through hardlinks across pods on the node.
