# Copyright 2026 The onggi authors. SPDX-License-Identifier: Apache-2.0
"""External artifact fixtures exercise the same checks without loading model files into RAM."""
import contextlib
import copy
import hashlib
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import conformance


class RuntimeFixtureTests(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.root = self.work.name
        self.old_fails = conformance.FAILS[:]
        conformance.FAILS[:] = []
        self.addCleanup(lambda: conformance.FAILS.__setitem__(slice(None), self.old_fails))
        self.base = b'a' * 16384
        self.target = self.base[:4096] + b'b' * 100 + self.base[4196:]
        for name, data in [('base', self.base), ('target', self.target)]:
            os.mkdir(os.path.join(self.root, name))
            with open(os.path.join(self.root, name, 'model.bin'), 'wb') as f:
                f.write(data)
        self.switch = {
            'schema': 'onggi.switchplan.v1', 'base_manifest': 'sha256:' + '1' * 64,
            'manifest': 'sha256:' + '2' * 64, 'size': 16384, 'same': 16284, 'moved': 0, 'new': 100,
            'files': [{'path': 'model.bin', 'base': 'model.bin', 'size': 16384,
                       'same': 16284, 'moved': 0, 'new': 100,
                       'ranges': [{'offset': 0, 'length': 4096, 'kind': 'same'},
                                  {'offset': 4096, 'length': 100, 'kind': 'new'},
                                  {'offset': 4196, 'length': 12188, 'kind': 'same'}]}]}
        self.case = {'name': 'native-inplace', 'base_dir': 'base', 'target_dir': 'target',
                     'switchplan': self.switch, 'expect_mode': 'units'}
        self.fixture = {'schema': 'onggi.binding-fixtures.v1', 'cases': [self.case]}
        self.fixture_path = os.path.join(self.root, 'fixtures.json')

    def write_fixture(self, fixture=None):
        with open(self.fixture_path, 'w') as f:
            json.dump(self.fixture if fixture is None else fixture, f)

    def cli(self, *args, fixture=None, env=None):
        self.write_fixture(fixture)
        return subprocess.run([sys.executable, conformance.__file__, '--fixtures', self.fixture_path] + list(args),
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)

    def test_external_units_and_report_record_inputs_and_commands(self):
        report = os.path.join(self.root, 'report.json')
        result = self.cli('--report', report)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(os.path.isfile(report), 'external fixture run must write the requested report')
        with open(report) as f:
            doc = json.load(f)
        self.assertTrue(doc['ok'])
        with open(self.fixture_path, 'rb') as f:
            self.assertEqual(doc['fixtures']['sha256'], hashlib.sha256(f.read()).hexdigest())
        case = doc['cases'][0]
        self.assertEqual(case['name'], 'native-inplace')
        self.assertEqual(case['target_files']['model.bin']['sha256'], hashlib.sha256(self.target).hexdigest())
        self.assertEqual(case['target_files']['model.bin']['size'], 16384)
        operations = [r['command'][-1] for r in case['results'] if r['command'][-1] in ('resident', 'rollback')]
        self.assertIn('resident', operations)
        self.assertEqual(operations.count('rollback'), 2)
        self.assertIn(b'== native-inplace', result.stdout)
        self.assertNotIn(b'== shifted', result.stdout)

    def test_path_switchplan_and_b1_reload(self):
        switchpath = os.path.join(self.root, 'plan.json')
        with open(switchpath, 'w') as f:
            json.dump(self.switch, f)
        case = dict(self.case, switchplan='plan.json', expect_mode='reload')
        result = self.cli('--level', 'B1', fixture=dict(self.fixture, cases=[case]))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(b'== native-inplace', result.stdout)
        self.assertNotIn(b'== shifted', result.stdout)

    def test_b2_reload_verifies_apply_and_rollback(self):
        switch = copy.deepcopy(self.switch)
        switch.update(same=0, new=16384)
        switch['files'][0].update(same=0, new=16384,
                                  ranges=[{'offset': 0, 'length': 16384, 'kind': 'new'}])
        case = dict(self.case, name='native-reload', switchplan=switch, expect_mode='reload')
        result = self.cli(fixture=dict(self.fixture, cases=[self.case, case]))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(b'expected mode units', result.stdout)
        self.assertIn(b'expected mode reload', result.stdout)
        self.assertEqual(result.stdout.count(b'rollback restores'), 2)

    def test_b2_requires_a_changed_switch_expected_to_use_units(self):
        identical = copy.deepcopy(self.switch)
        identical.update(same=16384, new=0)
        identical['files'][0].update(same=16384, new=0,
                                     ranges=[{'offset': 0, 'length': 16384, 'kind': 'same'}])
        no_write = dict(self.case, name='native-identical', target_dir='base', switchplan=identical)
        reload_case = dict(self.case, expect_mode='reload')
        for cases in [[reload_case], [no_write], [no_write, reload_case]]:
            with self.subTest(cases=cases):
                self.write_fixture(dict(self.fixture, cases=cases))
                with self.assertRaisesRegex(ValueError, 'B2 requires.*changed.*units'):
                    conformance.load_fixtures(self.fixture_path, 'B2')

    def test_rejects_false_same_and_elsewhere_byte_claims(self):
        for kind, counter in [('same', 'same'), ('elsewhere', 'moved')]:
            switch = copy.deepcopy(self.switch)
            switch['new'] = switch['files'][0]['new'] = 0
            switch[counter] += 100
            switch['files'][0][counter] += 100
            middle = switch['files'][0]['ranges'][1]
            middle['kind'] = kind
            if kind == 'elsewhere':
                middle.update(source_file='model.bin', source_offset=0)
            case = dict(self.case, switchplan=switch, expect_mode='reload')
            with self.subTest(kind=kind):
                self.write_fixture(dict(self.fixture, cases=[case]))
                with self.assertRaisesRegex(ValueError, kind + ' range bytes differ'):
                    conformance.load_fixtures(self.fixture_path, 'B1')

    def test_b2_rejects_identical_bytes_reported_as_new(self):
        case = dict(self.case, target_dir='base')
        self.write_fixture(dict(self.fixture, cases=[case]))
        with self.assertRaisesRegex(ValueError, 'B2 requires.*changed.*units'):
            conformance.load_fixtures(self.fixture_path, 'B2')

    def test_accepts_true_elsewhere_bytes_with_changed_units(self):
        for directory, data in [('base', self.base), ('target', self.target)]:
            data = data[:8192] + b'b' * 100 + data[8292:]
            with open(os.path.join(self.root, directory, 'model.bin'), 'wb') as f:
                f.write(data)
        self.switch['new'] = self.switch['files'][0]['new'] = 0
        self.switch['moved'] = self.switch['files'][0]['moved'] = 100
        self.switch['files'][0]['ranges'][1].update(kind='elsewhere', source_file='model.bin', source_offset=8192)
        result = self.cli()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_fixture_expectation_cannot_be_silently_downgraded(self):
        env = dict(os.environ, ONGGI_FAKE_LEVEL='B1')
        result = self.cli(env=env)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b'expected mode units', result.stdout)

    def test_zero_base_manifest_still_probes_a_different_wrong_base(self):
        self.switch['base_manifest'] = 'sha256:' + '0' * 64
        result = self.cli()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_invalid_fixture_report_records_input_hash_and_failure(self):
        report = os.path.join(self.root, 'report.json')
        result = self.cli('--report', report, fixture=dict(self.fixture, cases=[]))
        self.assertNotEqual(result.returncode, 0)
        with open(report) as f:
            doc = json.load(f)
        self.assertFalse(doc['ok'])
        self.assertIn('fixtures', doc)
        self.assertEqual(doc['fixtures']['path'], self.fixture_path)
        with open(self.fixture_path, 'rb') as f:
            self.assertEqual(doc['fixtures']['sha256'], hashlib.sha256(f.read()).hexdigest())
        self.assertTrue(doc['failures'])

    def test_rejects_invalid_provider_before_running_binding(self):
        invalid = [dict(self.fixture, schema='unknown'), dict(self.fixture, cases=[]),
                   dict(self.fixture, cases=[self.case, self.case])]
        for changes in [{'name': ''}, {'base_dir': 'missing'}, {'target_dir': ''},
                        {'expect_mode': 'anything'}, {'switchplan': 'missing.json'}]:
            invalid.append(dict(self.fixture, cases=[dict(self.case, **changes)]))
        for changes in [{'manifest': 'no digest'}, {'schema': 'unknown'}, {'files': []}]:
            invalid.append(dict(self.fixture, cases=[dict(self.case, switchplan=dict(self.switch, **changes))]))
        for key, value in [('path', '../model.bin'), ('size', 123), ('ranges', []),
                           ('ranges', [{'offset': 1, 'length': 16383, 'kind': 'same'}])]:
            switch = copy.deepcopy(self.switch)
            switch['files'][0][key] = value
            invalid.append(dict(self.fixture, cases=[dict(self.case, switchplan=switch)]))
        for fixture in invalid:
            with self.subTest(fixture=fixture):
                result = self.cli(fixture=fixture)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertNotIn(b'== ', result.stdout)

    def test_report_refuses_to_overwrite_existing_file(self):
        report = os.path.join(self.root, 'report.json')
        with open(report, 'w') as f:
            f.write('keep')
        result = self.cli('--report', report)
        self.assertNotEqual(result.returncode, 0)
        with open(report) as f:
            self.assertEqual(f.read(), 'keep')
        self.assertNotIn(b'== ', result.stdout)

    def test_failed_mode_check_is_in_report(self):
        report = os.path.join(self.root, 'report.json')
        result = self.cli('--report', report, env=dict(os.environ, ONGGI_FAKE_LEVEL='B1'))
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(os.path.isfile(report))
        with open(report) as f:
            doc = json.load(f)
        self.assertFalse(doc['ok'])
        self.assertTrue(any('expected mode units' in failure for failure in doc['failures']))

    def test_verifies_disk_intervals_with_bounded_reads(self):
        path = os.path.join(self.root, 'large.bin')
        data = b'0123456789' * 300000
        with open(path, 'wb') as f:
            f.write(data)
        plan = {'mode': 'reload', 'manifest': 'sha256:' + '2' * 64, 'units': []}
        ver = {'schema': 'onggi.verification.v1', 'op': 'apply', 'mode': 'reload',
               'manifest': plan['manifest'], 'ok': True,
               'units': [{'file': 'model.bin', 'offset': 0, 'length': len(data),
                          'sha256': hashlib.sha256(data).hexdigest()}]}
        real_open = open

        class BoundedReader:
            def __init__(self, f):
                self.f = f
            def __enter__(self):
                return self
            def __exit__(self, *args):
                self.f.close()
            def seek(self, *args):
                return self.f.seek(*args)
            def read(self, n=-1):
                if not 0 <= n <= 1024 * 1024:
                    raise AssertionError('artifact reads must be bounded')
                return self.f.read(n)

        def bounded_open(filename, *args, **kwargs):
            f = real_open(filename, *args, **kwargs)
            return BoundedReader(f) if filename == path else f

        with mock.patch('builtins.open', side_effect=bounded_open), contextlib.redirect_stdout(io.StringIO()):
            self.assertTrue(conformance.intervals_equal(path, 0, path, 0, len(data)))
            self.assertFalse(conformance.intervals_equal(path, 0, path, 1, len(data) - 1))
            with self.assertRaisesRegex(ValueError, 'shortened'):
                conformance.intervals_equal(path, 0, path, 0, len(data) + 1)
            self.assertTrue(conformance.verify_units(ver, {'model.bin': path}, 'disk reload', plan, 'apply'))
            plan = dict(plan, mode='units', units=[{'file': 'model.bin', 'offset': 4096, 'length': 1200000}])
            ver = dict(ver, mode='units', units=[dict(plan['units'][0],
                       sha256=hashlib.sha256(data[4096:1204096]).hexdigest())])
            self.assertTrue(conformance.verify_units(ver, {'model.bin': path}, 'disk units', plan, 'apply'))
            ver['units'][0]['sha256'] = '0' * 64
            self.assertFalse(conformance.verify_units(ver, {'model.bin': path}, 'bad disk digest', plan, 'apply'))


if __name__ == '__main__':
    unittest.main()
