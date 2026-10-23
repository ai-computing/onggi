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

## Limits

No privileged CSI mount, live Kubernetes rollout, actual model or NPU-memory update was tested in this run.
The concurrency limit bounds active materializations, not absolute RSS. A process crash between lease and
volume-record writes can conservatively retain an extra lease; automatic crash reconciliation remains unimplemented.
