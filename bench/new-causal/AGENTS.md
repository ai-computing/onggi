# CPU causal evidence scope

Read README.md before editing/running. Real model cohorts begin `not_run`.
Keep changes inside this directory; do not modify runtime/core, manuscript,
NUMBERS, board state or historical raw results. Do not allocate a device,
install system software, use SSH or launch remote work here. The hub parent
owns authorized node execution through `hub.orca.run_remote`.

Freeze binary/source/input/store identities and all treatment choices before
smoke/main. Test on a new small scratch fixture first. Main requires matching
excluded smoke and five alternating paired P/R attempts; preserve failures,
fallback and hash mismatches, stop the dependent matrix, and use new output
paths for retries. Never replace actual rsync/xdelta execution with emulation.

Keep codec and frame treatments separate, and publication/deployment/validation
clocks separate. CLI bytes is target size. Actual local-I/O claims require the
validated traced diagnostic cohort; incomplete attribution means unavailable.
No traced/untraced timing pooling, physical-I/O inference or invented results.

Run `python3 -m unittest discover -s bench/new-causal -p test_causal.py -v`;
Linux strace and the explicit ONGGI_TEST_BIN gate exercise actual file/runtime
paths. Run existing Go checks where the existing Go environment is available;
record unavailable checks rather than installing tools or asserting success.
