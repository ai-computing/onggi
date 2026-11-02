"""Offline tests of retained real records; no model replay or remote work."""
import base64
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


if __name__ == '__main__':
    unittest.main()
