"""Offline tests of retained real records; no model replay or remote work."""
import base64
import copy
import gzip
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import tempfile
import unittest


HERE = Path(__file__).parent
spec = importlib.util.spec_from_file_location('verify_timing', HERE / 'verify_timing.py')
v = importlib.util.module_from_spec(spec)
spec.loader.exec_module(v)
DATA = HERE / 'data/20261102'


def resign(root):
    doc = json.loads(gzip.decompress((root / 'exact-evidence.json.gz').read_bytes()))
    for entry in doc['files']:
        raw = (root / entry['path']).read_bytes()
        entry.update(bytes=len(raw), sha256=hashlib.sha256(raw).hexdigest(), base64=base64.b64encode(raw).decode())
    packed = gzip.compress(json.dumps(doc).encode())
    (root / 'exact-evidence.json.gz').write_bytes(packed)
    manifest = json.loads((root / 'export-manifest.json').read_bytes())
    manifest['files'] = [{k: value for k, value in row.items() if k != 'base64'} for row in doc['files']]
    manifest['archive'].update(bytes=len(packed), sha256=hashlib.sha256(packed).hexdigest())
    (root / 'export-manifest.json').write_text(json.dumps(manifest), encoding='utf-8')


class TimingExportTests(unittest.TestCase):
    def test_complete_main_and_excluded_smoke(self):
        for label, n, trials in [('timing-smoke', 1, 5), ('timing-main', 5, 25)]:
            result = v.verify(DATA / label, label)
            self.assertTrue(result['verified'])
            self.assertEqual((result['n'], result['trials']), (n, trials))

    def test_false_validated_clock_is_rejected_even_with_resigned_export(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'export'
            shutil.copytree(DATA / 'timing-smoke', root)
            cohort_file = root / 'timing-smoke/cohort.json'
            cohort = json.loads(cohort_file.read_bytes())
            cohort['trials'][0]['validated_s'] += 1
            summary = cohort['summary']['P']
            summary['samples'][0]['validated_s'] += 1
            summary['validated_s']['values'][0] += 1
            for key in ('median', 'q1', 'q3'):
                summary['validated_s'][key] += 1
            cohort_file.write_text(json.dumps(cohort), encoding='utf-8')
            row = {k: value for k, value in cohort['trials'][0].items() if k != 'run_id'}
            (root / 'timing-smoke/block-01-P/result.json').write_text(json.dumps(row), encoding='utf-8')
            doc = json.loads(gzip.decompress((root / 'exact-evidence.json.gz').read_bytes()))
            for entry in doc['files']:
                raw = (root / entry['path']).read_bytes()
                entry.update(bytes=len(raw), sha256=hashlib.sha256(raw).hexdigest(),
                             base64=base64.b64encode(raw).decode())
            packed = gzip.compress(json.dumps(doc).encode())
            (root / 'exact-evidence.json.gz').write_bytes(packed)
            manifest = json.loads((root / 'export-manifest.json').read_bytes())
            manifest['files'] = [{k: value for k, value in row.items() if k != 'base64'} for row in doc['files']]
            manifest['archive'].update(bytes=len(packed), sha256=hashlib.sha256(packed).hexdigest())
            (root / 'export-manifest.json').write_text(json.dumps(manifest), encoding='utf-8')
            with self.assertRaises(AssertionError):
                v.verify(root, 'timing-smoke')

    def test_control_records_prove_three_cpu_only_uid_cleanups(self):
        proof = v.verify_controls(DATA / 'control')
        self.assertEqual(proof['cpu_jobs_uid_cleaned'], 3)

    def test_shortened_command_clock_rejected_against_raw_gnu_time(self):
        self.damage_command('clock')

    def test_changed_store_rejected_even_with_matching_resource_header(self):
        self.damage_command('store')

    def damage_command(self, damage):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'export'
            shutil.copytree(DATA / 'timing-smoke', root)
            cohort_file = root / 'timing-smoke/cohort.json'
            cohort = json.loads(cohort_file.read_bytes())
            row = cohort['trials'][0]
            command = row['command']
            if damage == 'clock':
                command['elapsed_s'] = .1
                command['monotonic_end_ns'] = command['monotonic_start_ns'] + 100_000_000
                stats = cohort['summary']['P']['command_s']
                stats['values'] = [.1]
                for key in ('median', 'q1', 'q3'):
                    stats[key] = .1
            else:
                old, wrong = command['argv'][command['argv'].index('--store') + 1], '/tmp/unrelated-store'
                command['argv'] = [wrong if x == old else x for x in command['argv']]
                command['executed_argv'] = [wrong if x == old else x for x in command['executed_argv']]
                command['gnu_time'] = command['gnu_time'].replace(old, wrong)
                (root / 'timing-smoke/block-01-P/deploy.resources').write_text(command['gnu_time'], encoding='utf-8')
            cohort['summary']['P']['samples'][0] = copy.deepcopy(row)
            cohort_file.write_text(json.dumps(cohort), encoding='utf-8')
            trial = root / 'timing-smoke/block-01-P'
            (trial / 'deploy.json').write_text(json.dumps(command), encoding='utf-8')
            (trial / 'result.json').write_text(json.dumps({k: val for k, val in row.items() if k != 'run_id'}), encoding='utf-8')
            resign(root)
            with self.assertRaises(AssertionError):
                v.verify(root, 'timing-smoke')

    def test_actual_store_config_and_source_build_chain(self):
        proof = v.verify_provenance(DATA)
        self.assertEqual(proof['source_revision'], '05c97605b07882f9ceaaeb90b94878f8923f81cf')


if __name__ == '__main__':
    unittest.main()
