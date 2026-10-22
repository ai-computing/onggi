# CSI driver, v0 (#27)

The CSI driver of ADR-0004. A pod names a ref in an ephemeral inline volume, and the node plugin mounts that artifact
read-only. Pods with the same ref on a node share one copy of its files. Replace every `<...>` in the manifests.

- [`csidriver.yaml`](csidriver.yaml): the `CSIDriver` object `onggi.ai-computing.github.io`. Only ephemeral volumes;
  `podInfoOnMount`; no attach.
- [`node-plugin.yaml`](node-plugin.yaml): a DaemonSet with the plugin (`onggi csi`) and the
  `csi-node-driver-registrar` sidecar. The plugin does not call the Kubernetes API, so there is no RBAC.
- [`example-pod.yaml`](example-pod.yaml): a pod with an onggi volume.

```yaml
volumes:
  - name: model
    csi:
      driver: onggi.ai-computing.github.io
      readOnly: true
      volumeAttributes: {ref: <ref>}
```

## How it works

On `NodePublishVolume` the plugin:
1. resolves the ref in its own store (`ONGGI_STORE`, credentials from its env or a mounted secret; pods name only a
   ref);
2. materializes the manifest once per node, into `<data dir>/artifacts/<manifest digest>`:
   - the pull goes through the node cache `<data dir>/cache`, so a variant reads from the store only what the node
     lacks (#23);
   - it writes into a directory under `<data dir>/tmp`, which is renamed into place when complete;
   - concurrent requests for one manifest share one pull;
3. records a lease (`<data dir>/leases/<manifest>/<volume>`);
4. bind-mounts the directory read-only (`nosuid,nodev`) at the pod's target path.

The pod stays in `ContainerCreating` until this returns. kubelet gives up on a call after about two minutes and
retries it. The pull keeps running in the meantime, and the retry joins it.

`NodeUnpublishVolume` unmounts and drops the lease. A ref moved to a new manifest gets a new directory; pods that
already run keep the old one.

## Eviction (#33)

Every `--gc-every` (default 1 h) the plugin:
1. deletes materialized artifacts that no lease holds and that no pod has published or released for `--evict-after`
   (default 24 h): unmount the self-bind, rename into `tmp/`, remove;
2. runs `onggi gc` on the node cache with the plugin's store as the roots store: chunks that the artifacts still
   materialized or pulled within `--keep-used` (default 168 h) need stay; the rest go after `--gc-grace` (default 1 h).

`--gc-every 0` turns both off.

## Limits of v0

- The files are a second copy next to the node cache, which holds packed and compressed chunks. Reflinks or
  hardlinks from the cache need a cache of plain files.
- One plugin per node and data directory. Linux only.
- The plugin neither serves its cache to peers nor asks them yet (`onggi peer serve`, `pull --peers`, #34); no metrics yet.
- The plugin runs privileged, with `Bidirectional` mount propagation on the kubelet's pods directory. This is what
  any CSI node plugin needs; run it in a namespace for system components.
- No onggi image is published yet; the manifests use `<image with the onggi binary>`.

## Tested (#27)

Lab cluster, one node (`npu-node-1`), a test namespace deleted afterwards. The store was the ATOM Llama-3.1-8B
two-level store on a read-only hostPath; the node cache started empty.

| step | result |
|---|---|
| two pods, decode B2 (15.2 GB), at once | one pull: 10.20 GB read from the store in 2 min 48 s. kubelet timed out the first calls after 2 min; the retries joined the running pull. Both pods saw the same inode; `cmp` with the source: identical in both |
| writes | `touch` fails with `Read-only file system`, also in a pod that mounts the volume without `readOnly` |
| pod with decode B8 (15.2 GB) | 28.3 MB from the store (0.28 % of 10.2 GB stored), 10.18 GB from the node cache, 1 min 22 s; identical to the source |
| delete the pods | every unpublish succeeded; no mount of the volumes or of the data directory left in the node's mountinfo; leases 0 |

The first build remounted only the target read-only. Inside the pods the volume was read-only, but the host's
mountinfo showed `rw`: a remount does not propagate to the host. The plugin now binds from a read-only self-bind of
the artifact, and the host shows `ro,nosuid,nodev`.

## Tested (#33)

Same node and store, with `--gc-every 1m --evict-after 2m --keep-used 3m --gc-grace 30s` and an empty node cache.

| step | result |
|---|---|
| pods with decode B2, then B8 | 10.20 GB, then 28.3 MB read from the store; GC rounds during the pulls deleted nothing (the pulls hold cache leases) |
| delete the B8 pod | its artifact evicted 2 min 35 s later; the B2 pod's volume still mounted and identical to the source |
| node-cache GC | after `--keep-used` ran out: 2 objects (29.7 MB) deleted two rounds later; the cache is back to what B2 needs (10.20 GB) |
| new B8 pod | 28.3 MB from the store, 10.18 GB from the cache, 59 s; identical to the source; read-only |
| delete the pods | every unpublish succeeded; no leases, volume mounts or files left |
