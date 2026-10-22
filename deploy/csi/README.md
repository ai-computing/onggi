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

## Peers (#37)

With `--peer-listen :7070` the plugin serves its node cache to other nodes (`onggi peer serve`, #34). With
`--peers-dns onggi-csi-peers.<namespace>.svc.cluster.local:7070` it finds the other plugins through the headless
Service in `node-plugin.yaml`, re-resolving every 30 s and leaving out its own `POD_IP`. A pull then asks the peers
for the chunks the node cache lacks before the store; every chunk is hash-checked, and the store serves what peers do
not. `--peers URL,...` adds a static list. The peer port serves read-only chunks without authentication; keep it
inside the cluster.

## Delta apply (#41)

When a pod needs an artifact the node does not have, the plugin offers the pull the artifacts that no volume has used
for `--rebase-idle` (default 5 min). The pull picks the one that keeps the most bytes at the same offsets (at least half),
takes its directory out of `artifacts/` (after unmounting its self-bind), moves its files into place and writes only
the chunks that differ. Between two decode variants that is about 1.4 % of 15 GB (`onggi diff`, #38). The consumed
artifact is gone from the node; if a pod asks for it again, it comes back the same way. `--rebase-idle 0` turns this
off.

When the best idle artifact keeps too little at the same offsets but holds the bytes elsewhere (prefill after decode,
or a TensorRT engine whose weights moved), the pull reads those bytes from it where it is and leaves it in place (#58).

On the lab node (`onggi pull --base`, the same pull the plugin runs):

| decode B8 (15.2 GB), node holds decode B2 | read | left in place | time | check |
|---|---|---|---|---|
| plain pull through the node cache | 28.3 MB store + 10.18 GB cache | 0 | 73.8 s | identical |
| from the B2 copy, through the node cache | 28.3 MB cache | 15.01 GB | **8.9 s** (26.8 s before #42) | identical |
| from the B2 copy, no node cache | 28.3 MB store | 15.01 GB | **5.7 s** | identical |
| back to B2 from the B8 copy, through the node cache | 23.2 MB cache | 15.01 GB | 8.1 s (23.5 s before #42) | identical |

Through the node cache, a pull that needs only a few chunks finds them by scanning the cache's pack indexes instead of
loading its whole index (3.3 M chunks), and index lookups search a contiguous array of digest prefixes (#42).

In the lab cluster (#41; `npu-node-1`, `--rebase-idle 1m`): a pod with decode B8 was Running 12 s after creation,
taking over the idle B2 artifact (15.0 GB left in place, 28.3 MB read, 7.5 s pull; byte-identical, read-only), against
about a minute before. A B2 pod started while the B8 pod ran did not touch the B8 artifact. A prefill pod took over
nothing: the plugin logs why (`took over nothing: ... keeps x %` or `no artifact to take over`, #49).

## Limits of v0

- The files are a second copy next to the node cache, which holds packed and compressed chunks. Reflinks or
  hardlinks from the cache need a cache of plain files.
- One plugin per node and data directory. Linux only.
- No metrics yet. Peers are every plugin behind the Service; there is no placement or replication policy yet.
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

## Tested (#37)

Two nodes, `npu-node-1` and `cpu-1`, in a test namespace deleted afterwards. The store was the same ATOM store, served over S3 (VersityGW) from `npu-node-1`. Both plugins used `--peer-listen=:7070` and `--peers-dns` on the headless Service.

| step | store | node cache | peers | time | check |
|---|---|---|---|---|---|
| `npu-node-1`, decode B2 | 10.20 GB | 0 | 0 | 4 min 58 s | identical to the source |
| `cpu-1`, decode B2 | **0 B** | 0 | 10.20 GB (3,272,372 chunks) | 2 min 24 s | identical; read-only |
| `cpu-1`, decode B8 | 28.3 MB (0.28 %) | 10.18 GB | 0 | 1 min 26 s | identical |

`npu-node-1` never had B8, so the chunks that only B8 uses came from the store. The cold pull over S3 took longer than from a local directory store (about 2.5 min); kubelet's calls timed out twice, and the retries joined the running pull.
