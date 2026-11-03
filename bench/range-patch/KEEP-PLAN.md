# Diff-to-patch preparation

Continuation of issue95/PR96; real-model timing remains **not_run**.
The user has authorized continuing the experiment implementation and verification.

Use the existing measured-core `onggi diff --ranges --json` FileDiff format.
Do not add another matcher, SDK parser, controller, remote call or dependency.
The alternative is manual keep-plan transcription, which introduces unrecorded
decisions; decoding internal chunk-list objects would duplicate the store planner.
The chosen adapter complements the public changed ranges for an explicitly named
base/target file pair. It validates counters, range coverage, bounds and pinned
diff identity. Unknown schema versions are refused rather than guessed.

Preparation requires independently pinned complete base/target SHA256s and a
diff SHA256 supplied by the caller. Metadata identity does not prove byte equality:
the existing literal-patch preparation compares all kept bytes and hashes the
complete inputs. Original inputs remain immutable. A new package contains the
keep plan, its SHA and a receipt binding the input/diff/adapter identities and
actual preparation costs. It never overwrites an output or launches application.

Implementation plan (inline, Python3.10+ stdlib):

1. Add real-file tests for conversion plus patch application, moved-only/growth/
   shrink/identity/empty cases, wrong pair/hash, omitted ranges, malformed or
   inconsistent counters, out-of-base keeps and CLI failures. Observe RED.
2. Implement `diff_keep.py` with `derive_keep(document, base_path, target_path,
   base_size, target_size)` and pinned `prepare(...)`; expose one CLI.
3. Run both the existing reference suite and new tests. Add a Linux CI fixture
   that invokes the real CLI diff producer and this adapter, if the existing
   integration fixture supplies the needed synthetic artifacts.
4. Review the complete change, preserve evidence and update the draft PR. Do not
   infer model latency from fixtures. Remote model execution waits for the sole
   live transport owner and a separately frozen matched-contract cohort.

Fairness: record diff publication and extra full-input verification separately
from patch construction and later online application. Raw literals remain a
direct-executor control, not a codec-matched storage superiority experiment.
