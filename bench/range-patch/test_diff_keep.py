"""Pinned public diff metadata must produce a byte-verified patch, never guesses."""
import copy
import hashlib
import json
import os
from pathlib import Path
import random
import subprocess
import sys
import tempfile
import unittest

import diff_keep as bridge
import literal_patch as patch


def document(size=12, same=8, changed=None):
    changed = [[4, 4]] if changed is None else changed
    return dict(files=[dict(path='target.bin', base='base.bin', size=size,
                           same=same, moved=0, new=size-same,
                           changed_runs=len(changed), largest_run=max((r[1] for r in changed), default=0),
                           changed=changed)])


class DiffKeepTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.base = self.root/'base.bin'
        self.target = self.root/'target.bin'
        self.diff = self.root/'diff.json'
        self.output = self.root/'package'
        self.base.write_bytes(b'AAAABBBBCCCC')
        self.target.write_bytes(b'AAAAxxxxCCCC')
        self.diff.write_text(json.dumps(document()), encoding='utf-8')

    def options(self):
        return dict(diff_sha256=hashlib.sha256(self.diff.read_bytes()).hexdigest(),
                    base_path='base.bin', target_path='target.bin',
                    base_sha256=hashlib.sha256(self.base.read_bytes()).hexdigest(),
                    target_sha256=hashlib.sha256(self.target.read_bytes()).hexdigest())

    def test_complement_keeps_only_same_offsets_and_ignores_unrelated_file(self):
        doc = document()
        doc['files'].append(dict(path='other.bin'))
        self.assertEqual(bridge.derive_keep(doc, 'base.bin', 'target.bin', 12, 12), [[0, 4], [8, 4]])

    def test_growth_shrink_identity_empty_and_moved_only(self):
        cases = [
            (8, 12, 8, [[8, 4]], [[0, 8]]),
            (12, 4, 4, [], [[0, 4]]),
            (12, 12, 12, [], [[0, 12]]),
            (12, 0, 0, [], []),
            (0, 4, 0, [[0, 4]], []),
            (12, 12, 0, [[0, 12]], []),
        ]
        for base_size, size, same, changed, expected in cases:
            with self.subTest(size=size, same=same):
                doc = document(size, same, changed)
                if not same:
                    doc['files'][0]['moved'], doc['files'][0]['new'] = size, 0
                self.assertEqual(bridge.derive_keep(doc, 'base.bin', 'target.bin', base_size, size), expected)

    def test_missing_ranges_cannot_turn_changed_bytes_into_keep(self):
        doc = document()
        del doc['files'][0]['changed']
        with self.assertRaises(ValueError):
            bridge.derive_keep(doc, 'base.bin', 'target.bin', 12, 12)
        identical = document(12, 12, [])
        del identical['files'][0]['changed']
        self.assertEqual(bridge.derive_keep(identical, 'base.bin', 'target.bin', 12, 12), [[0, 12]])

    def test_wrong_pair_duplicate_path_unknown_schema_and_wrong_size_refused(self):
        bad = []
        for key, value in [('base', 'wrong.bin'), ('path', 'wrong.bin'), ('size', 13)]:
            doc = document()
            doc['files'][0][key] = value
            bad.append(doc)
        doc = document()
        doc['files'].append(copy.deepcopy(doc['files'][0]))
        bad.append(doc)
        doc = document()
        doc['schema'] = 'onggi.switchplan.v1'
        bad.append(doc)
        for doc in bad:
            with self.subTest(doc=doc), self.assertRaises(ValueError):
                bridge.derive_keep(doc, 'base.bin', 'target.bin', 12, 12)

    def test_bad_ranges_counters_and_out_of_base_keeps_refused(self):
        bad = []
        for ranges in ([[True, 4]], [[-1, 4]], [[4, 0]], [[4, 9]], [[4, 3], [6, 1]], [[8, 1], [4, 1]]):
            doc = document()
            doc['files'][0]['changed'] = ranges
            bad.append(doc)
        for key, value in [('same', 7), ('new', 3), ('new', True), ('changed_runs', 0), ('largest_run', 3), ('moved', -1)]:
            doc = document()
            doc['files'][0][key] = value
            bad.append(doc)
        for doc in bad:
            with self.subTest(doc=doc), self.assertRaises(ValueError):
                bridge.derive_keep(doc, 'base.bin', 'target.bin', 12, 12)
        with self.assertRaises(ValueError):
            bridge.derive_keep(document(), 'base.bin', 'target.bin', 8, 12)

    def test_prepare_binds_metadata_inputs_keep_and_source_then_real_apply(self):
        receipt = bridge.prepare(self.base, self.target, self.diff, self.output, **self.options())
        saved = json.loads((self.output/'preparation.json').read_bytes())
        self.assertEqual(saved, receipt)
        self.assertEqual(receipt['diff_sha256'], self.options()['diff_sha256'])
        self.assertEqual(receipt['keep_sha256'], hashlib.sha256((self.output/'keep.json').read_bytes()).hexdigest())
        self.assertEqual(receipt['adapter_sha256'], hashlib.sha256(Path(bridge.__file__).read_bytes()).hexdigest())
        self.assertEqual(receipt['reference_sha256'], hashlib.sha256(Path(patch.__file__).read_bytes()).hexdigest())
        self.assertEqual(receipt['input_verification_read_bytes'], 24)
        self.assertGreaterEqual(receipt['preparation_total_seconds'], receipt['patch']['preparation_seconds'])
        result = patch.apply(self.base, self.output, self.root/'applied.bin',
                             manifest_sha256=receipt['patch']['manifest_sha256'], exclusive=True)
        self.assertEqual((self.root/'applied.bin').read_bytes(), self.target.read_bytes())
        self.assertEqual(result['literal_written_bytes'], 4)

    def test_changed_pins_refused_before_package_creation(self):
        for key in ['diff_sha256', 'base_sha256', 'target_sha256']:
            options = self.options()
            options[key] = '0'*64
            with self.subTest(key=key), self.assertRaises(ValueError):
                bridge.prepare(self.base, self.target, self.diff, self.output, **options)
            self.assertFalse(self.output.exists())

    def test_duplicate_json_key_and_untruthful_keep_preserve_inputs(self):
        self.diff.write_bytes(b'{"files":[],"files":[]}')
        with self.assertRaises(ValueError):
            bridge.prepare(self.base, self.target, self.diff, self.output, **self.options())
        self.diff.write_text(json.dumps(document(12, 12, [])), encoding='utf-8')
        with self.assertRaises(ValueError):
            bridge.prepare(self.base, self.target, self.diff, self.output, **self.options())
        self.assertEqual(self.base.read_bytes(), b'AAAABBBBCCCC')
        self.assertFalse((self.output/'manifest.json').exists())

    def test_real_cli_is_exclusive_and_reports_actual_pins(self):
        args = [sys.executable, '-B', str(Path(bridge.__file__)), '--base', str(self.base),
                '--target', str(self.target), '--diff', str(self.diff), '--output', str(self.output)]
        for key, value in self.options().items():
            args += ['--'+key.replace('_', '-'), value]
        result = subprocess.run(args, capture_output=True, check=True)
        self.assertEqual(json.loads(result.stdout)['patch']['literal_bytes'], 4)
        self.assertEqual(result.stderr, b'')
        again = subprocess.run(args, capture_output=True)
        self.assertNotEqual(again.returncode, 0)
        self.assertEqual(json.loads((self.output/'preparation.json').read_bytes()), json.loads(result.stdout))

    @unittest.skipUnless(os.environ.get('ONGGI_TEST_BIN'), 'real Go CLI supplied by Linux CI')
    def test_real_onggi_diff_producer_through_verified_patch_application(self):
        binary = str(Path(os.environ['ONGGI_TEST_BIN']).resolve(strict=True))
        raw = random.Random(95).randbytes(4 << 20)
        changed = bytearray(raw)
        changed[2 << 20:(2 << 20)+1024] = b'x'*1024
        self.base.write_bytes(raw)
        self.target.write_bytes(changed)
        store = self.root/'store'
        identities = []
        for index, source in enumerate([self.base, self.target]):
            directory = self.root/('source-'+str(index))
            directory.mkdir()
            (directory/source.name).write_bytes(source.read_bytes())
            pushed = subprocess.run([binary, 'push', '--store', str(store), '--ref', 'v'+str(index),
                                     '--json', '--quiet', str(directory)], check=True, capture_output=True)
            identities.append(json.loads(pushed.stdout)['manifest'])
        diff = subprocess.run([binary, 'diff', '--store', str(store), '--ranges', '--json', *identities],
                              check=True, capture_output=True)
        self.diff.write_bytes(diff.stdout)
        receipt = bridge.prepare(self.base, self.target, self.diff, self.output, **self.options())
        self.assertGreater(receipt['patch']['kept_bytes'], 0)
        self.assertGreater(receipt['patch']['literal_bytes'], 0)
        self.assertLess(receipt['patch']['literal_bytes'], len(raw))
        applied = self.root/'applied.bin'
        patch.apply(self.base, self.output, applied,
                    manifest_sha256=receipt['patch']['manifest_sha256'], exclusive=True)
        self.assertEqual(applied.read_bytes(), changed)


if __name__ == '__main__':
    unittest.main()
