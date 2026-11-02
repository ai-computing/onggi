# Prepared-state common-caller P/C/R timing

Issue #88; separate evidence branch from source/verifier
`1e3c74f3c61f8791eadbec704b17df10feb55241`. These records were measured
on 2026-11-02 UTC (2026-11-03 KST). No timing was rerun for publication.
The excluded synthetic fixture tests harness correctness; its measurements
are not model performance. Excluded model smoke has one attempt per arm;
model main has five observations per arm, fifteen total.

## Question and executed treatments

With the same complete target chunk cache available locally, how do cached
reconstruction, intact-base copying and admitted consumption differ?

| Arm | Common Go materializer input | Executed witness |
|---|---|---|
| P | No base offer | Zero claims, zero local-base copy/retain counters, separate target inode |
| C | Same truthful base offer, non-null refusing Claim | One refused claim, actual intact-base copy counter, donor SHA/inode preserved |
| R | Same offer, admitting Claim | One admitted claim, retained-byte counter, original donor inode becomes target |

All use the same caller, unchanged core, target manifest, encoding, eight
pull workers, Go1.25.1, GOMAXPROCS32, no GOMEMLIMIT override, and12-CPU/32-GiB
limits (requests4CPU/8GiB). Independent verified base and complete cache copies
precede each command, including P's unused base. Measurements use etri-001,
POSIX files and warm/uncontrolled OS cache. The complete application cache has
308 encoded objects/10,364,627,872B. Store pack-payload, peer payload and newly
cached chunk payload counters are zero for every arm. Metadata/local reads remain.

Base: `decode_S256_B2_lm.rbln`,15,238,278,850B, SHA256
`8919e9fea5c9fddc6f5941dbd80364790eeff1cc55b13c153c1a14991ee59332`.
Target: `decode_S256_B8_lm.rbln`,15,228,048,882B, SHA256
`304ff369a72c21afbbc84b7b09639503cd0e6327320a3ec23664d75e6c13460d`.

## Accepted untraced clocks

Median [inclusive IQR] in seconds, n=5 per arm. Endpoints share command start;
they are not additive stage medians. Donor/inode diagnostics follow target
verification and are outside these clocks.

| Arm | Command | Through target sync | Through complete target SHA |
|---|---:|---:|---:|
| P |47.838 [0.013]|47.840 [0.013]|59.302 [0.784]|
| C |33.831 [0.138]|33.832 [0.138]|46.768 [0.120]|
| R |6.119 [0.166]|6.120 [0.166]|14.670 [0.185]|

C/R verified-completion median ratio is3.187946, rounded3.19. This compares
actual alternatives in prepared state, not a pure write-only ablation or
service/lifecycle speedup. C verifies/copies content; R compares placement
and resolves unfinished content. C reports15,009,745,126B copied; R reports
15,009,857,021B retained. These materializer counters are **not independently
traced C/R syscall bytes**.

Predeclared blocks are PCR,RCP,CPR,RPC,PRC. No attempt/outlier is removed or
replaced. All15 have successful commands, expected treatments, complete
target-SHA witnesses and artifact-tree cleanup. Smoke uses PCR with the same freeze.

## Exact retained evidence and checks

`data/20261103-three-arm/manifest.json` pins41 exact files: fixture/smoke/main
export manifests, compressed archives and original verification stdout/stderr;
actual CPU Job/Pod creation/status/cleanup; staging and terminal controller
records; original Go build/vet/race-test/version/binary logs. No model/compiler
payload, kubeconfig or API key is included. Synthetic/model distinctions remain.

| Package | Archive SHA256 |
|---|---|
| Actual synthetic fixture |61c1b865e1db26f8851da80914e55ad625d48b32fca2864353ce131c7a8a4de0|
| Excluded model smoke |7618241b73110841cba1c2178285817e137f78be0a0ee9fae5096b18df7afcb1|
| Model main |60636614bf68d6653328d88e2deba59694ca163bbfaff032e64d22b48f3d643b|

Main includes97 records; smoke25; fixture48. Measured runner/caller source is
1e99504; verifier1e3c74f; core05c9760. Binary SHA256:
`4613b8936144f4a13e22e9e47a4df700a73062e44615a6438741bd3d14634a53`.
Model freeze SHA256:
`3587418712c4ca8c86dee4f29c4c5f2a449b6d0a95a35bbb01ccc3e1a5c0b97d`.

From repo root, without model files, remote access or a device:

```sh
python3 -B -m unittest discover -s bench/new-causal -p test_three_arm_retained.py -v
```

This authenticates retained bytes and verifier source, extracts into temporary
directories, validates archived source/config/environment/command/treatment/
hash/inode witnesses, reconstructs monotonic intervals and compares every
saved report field. It also checks pinned CPU Job/Pod ownership, resources,
successful exits/cleanup and source/build receipts. Full Python discovery
runs these offline checks in CI without repeating model measurements.
Consistency of retained receipts is not remote-execution attestation.
Separate read-only review accepted exact archives and build/control witnesses
within this narrow timing scope.

## Boundaries and unfinished evidence

Only one stable-layout ATOM file pair is tested. Independent prepared donors
and complete caches do not establish naturally useful donor state. Preparation
durations lack retained start timestamps and remain worker-reported, not
independently reconstructed lifecycle results. Model/output payload is absent;
replay authenticates recorded full hashes rather than independently rehashing
those absent bytes.

Separately completed traced three-arm workers await exact export/reanalysis
and are not included here. Earlier P/R traces, historical timing and native
results are separate cohorts; none supplies C's missing syscall I/O. No physical
disk/device transfer inference follows. Moved-layout controls, broader directed
transitions, matched full-family policy study, charged donor lifecycle and
useful residual deployment after native sharing remain open.
