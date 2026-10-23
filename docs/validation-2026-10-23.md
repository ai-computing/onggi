# CSI safety validation — 2026-10-23

Base: `b63d5022ae1586916d92fae37dcf9cdf51e58f77`. Linux: Ubuntu 24.04 under WSL, amd64.

## Regression evidence

- Failed volume-record writes previously left one lease behind; the regression failed with `failed acquisition left 1 leases`.
- Rebinding a volume ID to a different manifest/target was accepted; the regression now checks rejection and preservation of the original volume.
- Three distinct manifests previously entered their pullers together. The regression now checks default/explicit concurrency limits of one and two, while the existing suite covers single-flight retries and callers that time out.

## Verification

Go **1.24.13**, matching the module's minimum version, and VersityGW **v1.8.0** bound to loopback with temporary test credentials/data:

```sh
export ONGGI_TEST_S3='s3://onggitest?endpoint=127.0.0.1:7070&insecure=1'
# Set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY to the local server's test credentials.
go vet ./...
go test -race -count=1 -json ./...
go build -o onggi ./cmd/onggi
```

Result: **113 test cases passed**, including subtests; no test case skipped or failed. Three packages contain no tests.
The suite also passed on Go 1.27.3 without the S3 server, with nine S3 subtests skipped in that earlier run.
`gofmt` and `git diff --check` passed.

An additional CLI round trip pushed two 4 MiB synthetic artifacts to S3, repeated a push, restored the base,
then pulled the changed artifact using `--base`/`--base-dir`. Both restores were byte-identical. The delta
pull reused **4,119,698 bytes** and fetched **70,733 bytes**. These are integrity checks, not NPU performance measurements.

## Real Linux mount lifecycle

Source `ca0226464d209f3680ec0d1efcf7a637300cbffc` plus
[`TestLinuxMountLifecycle`](../pkg/csi/mount_integration_linux_test.go) was checked in a disposable
Kubernetes pod on `cpu-2`, Linux `7.0.0-38-generic` amd64, using `golang:1.24` (Go **1.24.13**).
The pod was privileged, with an `emptyDir` workspace and no service account token, host paths,
host PID/network namespace, or mount propagation. No CSI driver registration or DaemonSet was installed.

Run this test only in a disposable mount namespace with `CAP_SYS_ADMIN`:

```sh
ONGGI_TEST_MOUNTS=1 go test -race ./pkg/csi -run '^TestLinuxMountLifecycle$' -count=1 -v
```

Without `ONGGI_TEST_MOUNTS=1`, the Linux test skips. With it, an unavailable mount capability fails
the test. The test uses the real filesystem store, materializer, `NodePublishVolume`,
`NodeUnpublishVolume`, eviction, and Linux `NewMounter`; it does not use the fake mounter.

The real mount test passed: concurrent publishes pulled the artifact once, held two leases, and
exposed byte-identical files sharing an inode. Both targets and the source self-bind had
`readonly,nosuid,nodev` flags, and writes failed with `EROFS`. Publish and unpublish retries succeeded.
A remaining lease prevented eviction; after the final unpublish, eviction removed the source
self-bind, artifact, and lease directory. Cleanup also checks `/proc/self/mountinfo` for test mounts
before temporary-directory removal.

The full CSI package also passed with `ONGGI_TEST_MOUNTS=1 go test -race ./pkg/csi -count=1 -v`:
**22 top-level tests passed**, none skipped, in 10.794 s. A negative control using
`setpriv --bounding-set=-sys_admin env ONGGI_TEST_MOUNTS=1 go test ./pkg/csi -run '^TestLinuxMountLifecycle$' -count=1 -v`
failed with `operation not permitted` and exit 1, confirming that an explicitly enabled test cannot
silently skip without mount privileges. `gofmt` and `git diff --check` passed. The final container
mount table contained **zero** integration-test mounts.

After checking resource UIDs, the test pod (`857e8cb0-beee-4be2-a30a-af5308b7cf62`) and namespace
`onggi-verify-20261023-8c6a12` (`79a84246-b6fb-4a9c-8c30-2365c48e15f2`) were deleted; the namespace
was confirmed absent. The cluster's CSI registrations remained NFS and SMB only.

## Limits

No live CSI driver rollout, kubelet registration, host mount propagation, actual model, or NPU-memory
update was tested. The isolated Linux test covers the node service and real mounts within one container.
The concurrency limit bounds active materializations, not absolute RSS. A process crash between lease and
volume-record writes can conservatively retain an extra lease; automatic crash reconciliation remains unimplemented.
