# Prepared literal patch reference

Benchmark-only Python3.10+ stdlib code; read README.md before editing/running.
No production/core/runtime changes, model parsing, installation, node access,
controller or device allocation. Real-model status starts not_run.

Use real temporary files for functional/failure tests. Preserve failed attempts
and original evidence; never generate model timings or silently replace a
baseline. Ownership is a truthful caller precondition, not an implemented CSI
lease detector. Keep preparation/application/full verification scopes explicit.

Run: python3 -B -m unittest discover -s bench/range-patch -v
