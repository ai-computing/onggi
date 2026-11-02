# Accepted CPU control cohorts (2026-11-02)

Raw main records are now exactly exported and independently reparsed. These cohorts are separate from the untraced prepared-artifact timing report. No model was recompiled and no accelerator was allocated. All CPU Job UID cleanup controls pass.

## Equal-complete-target-cache I/O

Five alternating P/R pairs, excluded matching smoke. Both receive independently verified copies of the same complete target-seeded cache (308 files,10,364,627,872B), store and immutable input identities. Both have independent verified base copies; only R offers/consumes the base. Setup copies/source warming precede the command. Every worker records full-output SHA inventory equality.

| Incremental command logical I/O | Cached reconstruction P | Consumed base R |
|---|---:|---:|
| Target writes | 15,228,048,882 B | 218,191,861 B |
| Cache reads | 10,364,627,831 B | 182,533,770 B |
| Store reads, including metadata | 212,367,401 B | 424,406,626 B |
| Private-staging writes | 0 B | 0 B |

All per-arm byte values above are identical across five repetitions. Reparsed target writes are98.567% lower for R. Both CLI pack-payload reads and peer bytes are zero; store-file reads are not zero. These are successful syscall return lengths during the deployment command, not physical disk/network/device traffic. Preparation copies and common post-command sync/fullSHA are excluded. Traced latencies are not pooled with untraced observations. Intact-base-copy and moved-layout controls are absent; this result does not establish complete lifecycle utility or all causal alternatives.

Exact archive:458 records/376 trace files;193,780,781 compressed bytes; SHA256 `7eca85ceb87445cd83a2c067a8e8d427d28cd37a9e70336081de4c525b2c59be`. Split parts are each below48MB. Raw traces remain compressed in Git. Independent replay matched every returned scientific report field.

## Two-file publication

Five configurations, five rotating repetitions each, fresh store per trial, A then B. C/F/T use zstd and no shared frames; C coarse, F flat fine, T two-level. TF adds256KiB frames to T; TA switches codec selection to auto with the same hierarchy/frames. Pack size64MiB, CPU12/memory32Gi, eight workers, GOMAXPROCS32 are fixed.

| Arm | Finalized GB | Metadata MB | A+B publication median s [IQR] |
|---|---:|---:|---:|
| C | 15.066525 | 46.342510 | 55.165610 [0.222374] |
| F | 15.799116 | 740.232823 | 118.498539 [0.980229] |
| T | 15.730762 | 671.866207 | 135.916223 [7.919369] |
| TF | 15.687486 | 673.590162 | 154.429036 [3.923655] |
| TA | 10.666609 | 750.652870 | 281.400724 [9.517656] |

F/T pack totals differ only12,656B; T saves68,366,616B metadata but has longer family publication median. C is smaller/faster than frame-off F/T on this stable-layout pair. TF/TA isolates codec-policy change at fixed hierarchy+frames, not two-level matching. There is no flat-fine-with-frames arm and no full-set causal isolation. Inspection/roundtrip validation are outside push timers; A+B totals are formed per trial before summarizing.

Exact main archive SHA256 `2b01ed3375152a6e3e70195c281bdae01f6fe70b50e7f249dbf51e0d0181fcb8`. Matching smoke is retained separately. Every consumed record, fixed settings, command/environment, clocks, inventories/push counters and summaries passed the verifier. Both artifacts roundtrip in every worker trial; absent model/output payloads cannot be independently rehashed offline.

## Verification provenance

Acceptance verifier commit `a45b292d5fcffe94e1ab48e43ddfbce376c2d307`. Checked frozen source is now compiled directly in memory in both timing/provenance paths, bypassing disk bytecode caches. Two mutations reproduced execution of unarchived matching bytecode in the old loader (RED2), then pass with direct compile. Current local suite:50 tests,46 pass4 Windows/Linux-or-CLI skips; Linux CI will verify the new head. No measured source or raw observation changed. Independent source review and direct-compile replays found no Important defect in the narrow scopes above.

Full exact acceptance outputs are `data/20261102/verification-{io-smoke,io-main,publish-main}.json`, with source and operational provenance. Source/binary remain core05c9760 / SHAe6b8ee52… / Go1.25.1, and frozen runner15468016…. No acceptance result implies real conference acceptance or closes unmeasured controls.
