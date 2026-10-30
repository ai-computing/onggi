# Copyright 2026 The onggi authors. SPDX-License-Identifier: Apache-2.0
"""Regression checks for the binding kit's verification boundary (stdlib only)."""
import contextlib
import copy
import hashlib
import io
import os
import tempfile
import unittest
from unittest import mock

import conformance


class VerificationTests(unittest.TestCase):
    def setUp(self):
        self.old_fails = conformance.FAILS[:]
        conformance.FAILS[:] = []

    def tearDown(self):
        conformance.FAILS[:] = self.old_fails

    def fixtures(self, mode="units", op="apply"):
        files = {"model.bin": b"abcdef", "config.json": b"cfg"}
        plan = {"mode": mode, "manifest": "sha256:" + "1" * 64,
                "base_manifest": "sha256:" + "0" * 64,
                "units": [{"file": "model.bin", "offset": 1, "length": 3},
                          {"file": "config.json", "offset": 0, "length": 3}] if mode == "units" else []}
        units = [dict(u, sha256=hashlib.sha256(files[u["file"]][u["offset"]:u["offset"] + u["length"]]).hexdigest())
                 for u in plan["units"]]
        ver = {"schema": "onggi.verification.v1", "op": op, "mode": mode,
               "manifest": plan["manifest" if op == "apply" else "base_manifest"],
               "ok": True, "units": units}
        return files, plan, ver

    def verify(self, ver, files, plan, op="apply"):
        with contextlib.redirect_stdout(io.StringIO()):
            return conformance.verify_units(ver, files, "verification", plan, op)

    def test_accepts_exact_planned_units_in_any_order(self):
        files, plan, ver = self.fixtures()
        ver["units"].reverse()
        self.assertTrue(self.verify(ver, files, plan))

    def test_rejects_missing_duplicate_and_unexpected_intervals(self):
        files, plan, ver = self.fixtures()
        units = ver["units"]
        extra = {"file": "model.bin", "offset": 0, "length": 1,
                 "sha256": hashlib.sha256(b"a").hexdigest()}
        for returned in [[], units[:1], units + units[:1], units + [extra],
                         [dict(units[0], offset=0, sha256=hashlib.sha256(b"abc").hexdigest()), units[1]]]:
            with self.subTest(units=returned):
                self.assertFalse(self.verify(dict(ver, units=returned), files, plan))

    def test_rejects_invalid_unit_fields_without_crashing(self):
        files, plan, ver = self.fixtures()
        bad = [{"offset": v} for v in [-1, True, 1.0, "1", None, 6]]
        bad += [{"length": v} for v in [0, -1, True, 3.0, "3", None, 99]]
        bad += [{"file": v} for v in ["unknown", None, [], 1]]
        bad += [{"sha256": v} for v in [None, "", "0" * 64, "Z" * 64]]
        for changes in bad:
            with self.subTest(changes=changes):
                units = [dict(ver["units"][0], **changes), ver["units"][1]]
                self.assertFalse(self.verify(dict(ver, units=units), files, plan))
        for units in [None, {}, [None], [{}]]:
            with self.subTest(units=units):
                self.assertFalse(self.verify(dict(ver, units=units), files, plan))
        for key in ["file", "offset", "length", "sha256"]:
            broken = copy.deepcopy(ver)
            del broken["units"][0][key]
            self.assertFalse(self.verify(broken, files, plan))

    def test_rejects_wrong_or_missing_verification_identity(self):
        files, plan, ver = self.fixtures()
        for key, wrong in [("schema", "unknown"), ("op", "rollback"), ("mode", "reload"),
                           ("manifest", plan["base_manifest"]), ("ok", 1), ("ok", False)]:
            with self.subTest(key=key, value=wrong):
                self.assertFalse(self.verify(dict(ver, **{key: wrong}), files, plan))
        for key in ver:
            missing = dict(ver)
            del missing[key]
            self.assertFalse(self.verify(missing, files, plan))
        for value in [None, [], True]:
            self.assertFalse(self.verify(value, files, plan))

    def test_checks_optional_bytes_written_when_present(self):
        files, plan, ver = self.fixtures()
        self.assertTrue(self.verify(dict(ver, bytes_written=6), files, plan))
        for value in [0, -1, True, 6.0, "6", None]:
            with self.subTest(bytes_written=value):
                self.assertFalse(self.verify(dict(ver, bytes_written=value), files, plan))

    def test_identical_switch_accepts_explicit_empty_units(self):
        files, plan, ver = self.fixtures()
        plan["units"] = []
        self.assertTrue(self.verify(dict(ver, units=[], bytes_written=0), files, plan))
        self.assertFalse(self.verify(ver, files, plan))
        missing = dict(ver)
        del missing["units"]
        self.assertFalse(self.verify(missing, files, plan))

    def test_reload_requires_every_whole_target_file(self):
        files, plan, ver = self.fixtures(mode="reload")
        units = [{"file": "model.bin", "offset": 0, "length": 6,
                  "sha256": hashlib.sha256(b"abcdef").hexdigest()},
                 {"file": "config.json", "offset": 0, "length": 3,
                  "sha256": hashlib.sha256(b"cfg").hexdigest()}]
        self.assertTrue(self.verify(dict(ver, units=units), files, plan))
        for returned in [[], units[:1], units + units[:1],
                         [dict(units[0], length=3, sha256=hashlib.sha256(b"abc").hexdigest()), units[1]]]:
            with self.subTest(units=returned):
                self.assertFalse(self.verify(dict(ver, units=returned), files, plan))

    def test_reload_rollback_uses_base_files_and_lengths(self):
        _, plan, ver = self.fixtures(mode="reload", op="rollback")
        base = {"model.bin": b"ab", "config.json": b"longer config", "base-only": b"old"}
        units = [{"file": name, "offset": 0, "length": len(data),
                  "sha256": hashlib.sha256(data).hexdigest()} for name, data in base.items()]
        self.assertTrue(self.verify(dict(ver, units=units), base, plan, "rollback"))
        self.assertFalse(self.verify(dict(ver, units=units[:2]), base, plan, "rollback"))
        self.assertFalse(self.verify(dict(ver, units=units, manifest=plan["manifest"]), base, plan, "rollback"))

    def test_units_rollback_requires_all_planned_base_intervals(self):
        _, plan, ver = self.fixtures(op="rollback")
        base = {"model.bin": b"012345", "config.json": b"old"}
        units = [{"file": "model.bin", "offset": 1, "length": 3,
                  "sha256": hashlib.sha256(b"123").hexdigest()},
                 {"file": "config.json", "offset": 0, "length": 3,
                  "sha256": hashlib.sha256(b"old").hexdigest()}]
        self.assertTrue(self.verify(dict(ver, units=units), base, plan, "rollback"))
        self.assertFalse(self.verify(dict(ver, units=units[:1]), base, plan, "rollback"))
        self.assertFalse(self.verify(dict(ver, units=units, op="apply"), base, plan, "rollback"))

    def test_rejects_empty_apply_verification_for_planned_units(self):
        real_run = conformance.run

        def omit_units(cmd, op, *args, **kwargs):
            rc, doc = real_run(cmd, op, *args, **kwargs)
            if op == "apply":
                doc["units"] = []
            return rc, doc

        with tempfile.TemporaryDirectory() as work, \
                mock.patch.dict(os.environ), \
                mock.patch.object(conformance.tempfile, "mkdtemp", return_value=work), \
                mock.patch.object(conformance.golden_data, "CASES", ("inplace",)), \
                mock.patch.object(conformance, "run", side_effect=omit_units), \
                contextlib.redirect_stdout(io.StringIO()):
            os.environ.pop("ONGGI_FAKE_LEVEL", None)
            self.assertEqual(conformance.main(["--level", "B2"]), 1)


if __name__ == "__main__":
    unittest.main()
