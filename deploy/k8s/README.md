# Kubernetes delivery, v0 (#25)

Until the CSI driver (ADR-0004) exists, artifacts reach pods through an init container.
[`pull-initcontainer.yaml`](pull-initcontainer.yaml) is a template; replace every `<...>`.

- The init container runs `onggi pull --cache /var/lib/onggi/cache <ref> /models/<ref>`.
- The **node cache** is a hostPath directory shared by every pod on the node. It is an ordinary onggi store. Chunks it holds are read locally; the rest are read from the store, hash-checked, and added to it. A second variant on a node reads only what the node lacks.
- The artifact goes into the pod's `emptyDir`, so it disappears with the pod; the cache stays.
- Several pods on one node may fill the cache at the same time. Objects are content-addressed and published atomically, so concurrent writers produce the same objects. #25 tested this on the lab cluster.
- Set `GOMEMLIMIT=1800MiB` so a pull stays under about 2 GB (README, Memory).

Limits of v0:
- Each pod gets its own copy of the artifact in its emptyDir, which a CSI volume will avoid.
- Nothing evicts from the cache yet (P3).
- There is no peer-to-peer fetch between nodes.
- Pods that write into a hostPath under a user's home should run as that user (`securityContext.runAsUser`), so the cache does not become root-owned.
- If pods run as a non-root user, create the cache directory beforehand as that user and use `type: Directory`. A `DirectoryOrCreate` hostPath is created by the kubelet as root, and the pull then fails with `mkdir /var/lib/onggi/cache/tmp: permission denied`. #25 hit this on its first run.
