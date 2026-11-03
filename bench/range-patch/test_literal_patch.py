"""Real-file checks for the deliberately simple prepared patch reference."""
import hashlib
import json
from pathlib import Path
import tempfile
import subprocess
import sys
import unittest
from unittest import mock

import literal_patch as patch


class LiteralPatchTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.base = self.root/'base.bin'
        self.target = self.root/'target.bin'
        self.package = self.root/'patch'
        self.output = self.root/'out.bin'

    def make(self, base, target, keep):
        self.base.write_bytes(base)
        self.target.write_bytes(target)
        result = patch.prepare(self.base, self.target, keep, self.package)
        self.identity = result['manifest_sha256']
        return result

    def apply(self, *, exclusive=True):
        return patch.apply(self.base, self.package, self.output, manifest_sha256=self.identity, exclusive=exclusive)

    def test_real_file_transitions_keep_target_identity_and_inode(self):
        cases = [
            (b'AAAABBBBCCCC', b'AAAAxxxxCCCC', [[0, 4], [8, 4]]),
            (b'AAAABBBBCCCC', b'BBBBCCCCAAAA', []),
            (b'AAAABBBB', b'AAAABBBBtail', [[0, 8]]),
            (b'AAAABBBBtail', b'AAAA', [[0, 4]]),
            (b'AAAABBBB', b'AAAABBBB', [[0, 8]]),
            (b'AAAA', b'', []),
            (b'', b'BBBB', []),
        ]
        for index, (base, target, keep) in enumerate(cases):
            with self.subTest(index=index):
                self.package = self.root/('patch'+str(index))
                self.output = self.root/('out'+str(index))
                prepared = self.make(base, target, keep)
                inode = self.base.stat()
                applied = self.apply()
                self.assertEqual(self.output.read_bytes(), target)
                self.assertFalse(self.base.exists())
                self.assertEqual((self.output.stat().st_dev, self.output.stat().st_ino), (inode.st_dev, inode.st_ino))
                self.assertEqual(applied['literal_written_bytes'], len(target)-sum(row[1] for row in keep))
                self.assertEqual(prepared['literal_bytes'], applied['literal_written_bytes'])
                self.assertEqual(applied['target_sha256'], hashlib.sha256(target).hexdigest())

    def test_untruthful_keep_is_rejected_without_committed_patch(self):
        self.base.write_bytes(b'AAAA')
        self.target.write_bytes(b'BAAA')
        with self.assertRaises(ValueError):
            patch.prepare(self.base, self.target, [[0, 4]], self.package)
        self.assertFalse((self.package/'manifest.json').exists())
        self.assertEqual(self.base.read_bytes(), b'AAAA')

    def test_invalid_keep_ranges_are_rejected(self):
        for keep in ([[True, 1]], [[0.0, 1]], [[-1, 1]], [[0, 0]], [[2, 2], [1, 1]], [[0, 3], [2, 1]], [[0, 5]]):
            with self.subTest(keep=keep):
                self.base.write_bytes(b'AAAA')
                self.target.write_bytes(b'AAAA')
                with self.assertRaises(ValueError):
                    patch.prepare(self.base, self.target, keep, self.package)

    def test_refusal_and_wrong_base_leave_paths_unchanged(self):
        self.make(b'AAAA', b'AABB', [[0, 2]])
        with self.assertRaises(ValueError):
            self.apply(exclusive=False)
        self.assertEqual(self.base.read_bytes(), b'AAAA')
        self.base.write_bytes(b'CCCC')
        with self.assertRaises(ValueError):
            self.apply()
        self.assertEqual(self.base.read_bytes(), b'CCCC')
        self.assertFalse(self.output.exists())

    def test_corrupt_truncated_or_extra_payload_rejects_before_mutation(self):
        self.make(b'AAAA', b'BBBB', [])
        payload = self.package/'payload.bin'
        for bad in (b'BBBC', b'BBB', b'BBBBx'):
            with self.subTest(payload=bad):
                payload.write_bytes(bad)
                with self.assertRaises(ValueError):
                    self.apply()
                self.assertEqual(self.base.read_bytes(), b'AAAA')
                self.assertFalse(self.output.exists())

    def test_manifest_range_corruption_is_rejected(self):
        self.make(b'AAAA', b'AABB', [[0, 2]])
        path = self.package/'manifest.json'
        document = json.loads(path.read_bytes())
        document['literals'][0]['offset'] = 0
        path.write_text(json.dumps(document), encoding='utf-8')
        self.identity = hashlib.sha256(path.read_bytes()).hexdigest()
        with self.assertRaises(ValueError):
            self.apply()
        self.assertEqual(self.base.read_bytes(), b'AAAA')

    def test_existing_output_and_package_are_not_overwritten(self):
        self.make(b'AAAA', b'BBBB', [])
        original = (self.package/'manifest.json').read_bytes()
        with self.assertRaises((ValueError, FileExistsError)):
            patch.prepare(self.base, self.target, [], self.package)
        self.assertEqual((self.package/'manifest.json').read_bytes(), original)
        self.output.write_bytes(b'consumer')
        with self.assertRaises((ValueError, FileExistsError)):
            self.apply()
        self.assertEqual(self.base.read_bytes(), b'AAAA')
        self.assertEqual(self.output.read_bytes(), b'consumer')

    def test_hardlinked_base_is_refused(self):
        self.make(b'AAAA', b'BBBB', [])
        alias = self.root/'other-consumer'
        alias.hardlink_to(self.base)
        with self.assertRaises(ValueError):
            self.apply()
        self.assertEqual(alias.read_bytes(), b'AAAA')

    def test_post_mutation_failure_is_not_published_or_rolled_back(self):
        self.make(b'AAAA', b'BBBB', [])
        with mock.patch.object(patch.os, 'fsync', side_effect=OSError('injected after mutation')):
            with self.assertRaises(patch.ApplyFailure) as failure:
                self.apply()
        self.assertFalse(self.output.exists())
        self.assertFalse(self.base.exists())
        self.assertEqual(failure.exception.work_file.read_bytes(), b'BBBB')

    def test_prepared_identity_binds_header_before_any_base_mutation(self):
        prepared = self.make(b'AAAA', b'BBBB', [])
        identity = prepared['manifest_sha256']
        path = self.package/'manifest.json'
        document = json.loads(path.read_bytes())
        document['target_sha256'] = hashlib.sha256(b'CCCC').hexdigest()
        path.write_text(json.dumps(document), encoding='utf-8')
        with self.assertRaises(ValueError):
            patch.apply(self.base, self.package, self.output, exclusive=True, manifest_sha256=identity)
        self.assertEqual(self.base.read_bytes(), b'AAAA')

    def test_cleanup_error_after_verified_publication_is_reported_as_committed(self):
        self.make(b'AAAA', b'BBBB', [])
        unlink = Path.unlink
        def refuse_work(path, *args, **kwargs):
            if path.name.startswith('.literal-patch-'):
                raise OSError('injected after commit')
            return unlink(path, *args, **kwargs)
        with mock.patch.object(Path, 'unlink', refuse_work):
            result = self.apply()
        self.assertEqual(self.output.read_bytes(), b'BBBB')
        self.assertEqual(result['cleanup_error'], 'injected after commit')
        self.assertEqual(Path(result['retained_work_file']).read_bytes(), b'BBBB')

    def test_multibuffer_literal_is_streamed_and_verified(self):
        unit = 1 << 20
        before = b'A'*(4*unit)
        after = before[:unit]+b'B'*(2*unit)+before[3*unit:]
        self.make(before, after, [[0, unit], [3*unit, unit]])
        result = self.apply()
        self.assertEqual(self.output.read_bytes(), after)
        self.assertEqual(result['literal_written_bytes'], 2*unit)
        self.assertEqual(result['target_validation_read_bytes'], len(after))

    def test_patch_payload_cannot_be_consumed_as_a_base(self):
        self.make(b'AAAA', b'BBBB', [])
        with self.assertRaises(ValueError):
            patch.apply(self.package/'payload.bin', self.package, self.output,
                        manifest_sha256=self.identity, exclusive=True)
        self.assertEqual((self.package/'payload.bin').read_bytes(), b'BBBB')
        self.assertEqual(self.base.read_bytes(), b'AAAA')

    def test_duplicate_json_keys_are_not_accepted_with_a_matching_digest(self):
        self.make(b'AAAA', b'BBBB', [])
        raw = b'{"schema":"x","schema":"y"}'
        (self.package/'manifest.json').write_bytes(raw)
        self.identity = hashlib.sha256(raw).hexdigest()
        with self.assertRaises(ValueError):
            self.apply()
        self.assertEqual(self.base.read_bytes(), b'AAAA')

    def test_racing_output_is_preserved_after_consumption(self):
        self.make(b'AAAA', b'BBBB', [])
        link = patch.os.link
        def race(source, destination):
            Path(destination).write_bytes(b'other consumer')
            return link(source, destination)
        with mock.patch.object(patch.os, 'link', race):
            with self.assertRaises(patch.ApplyFailure) as failure:
                self.apply()
        self.assertEqual(self.output.read_bytes(), b'other consumer')
        self.assertEqual(failure.exception.work_file.read_bytes(), b'BBBB')

    def test_real_cli_prepare_refusal_and_application(self):
        self.base.write_bytes(b'AAAABBBB')
        self.target.write_bytes(b'AAAAxxxx')
        plan = self.root/'keep.json'
        plan.write_text('[[0,4]]', encoding='utf-8')
        program = Path(patch.__file__).resolve()
        command = [sys.executable, '-B', str(program)]
        result = subprocess.run(command+['prepare', '--base', str(self.base), '--target', str(self.target),
                                '--keep', str(plan), '--output', str(self.package)], capture_output=True, check=True)
        prepared = json.loads(result.stdout)
        self.assertEqual(result.stderr, b'')
        self.assertEqual(prepared['source_sha256'], hashlib.sha256(program.read_bytes()).hexdigest())
        arguments = ['apply', '--base', str(self.base), '--patch', str(self.package), '--output', str(self.output),
                     '--manifest-sha256', prepared['result']['manifest_sha256']]
        refused = subprocess.run(command+arguments, capture_output=True)
        self.assertEqual(refused.returncode, 1)
        self.assertEqual(json.loads(refused.stderr)['status'], 'failed')
        self.assertEqual(self.base.read_bytes(), b'AAAABBBB')
        result = subprocess.run(command+arguments+['--exclusive'], capture_output=True, check=True)
        self.assertEqual(result.stderr, b'')
        self.assertEqual(json.loads(result.stdout)['status'], 'completed')
        self.assertEqual(self.output.read_bytes(), self.target.read_bytes())

    def test_phase_clock_stops_at_publication_before_cleanup(self):
        self.make(b'AAAA', b'BBBB', [])
        with mock.patch.object(patch.time, 'perf_counter', side_effect=[0.0, 1.0, 2.0, 5.0]):
            result = self.apply()
        self.assertEqual(result['mutation_to_publication_seconds'], 1.0)
        self.assertEqual(result['post_publication_cleanup_seconds'], 3.0)
        self.assertEqual(result['application_seconds'], 5.0)


if __name__ == '__main__':
    unittest.main()
