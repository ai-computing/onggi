"""Semantic mutation checks on the exact real-model smoke export, without reruns."""
import base64
import gzip
import hashlib
import importlib.util
import json
import marshal
from pathlib import Path
import shutil
import struct
import tempfile
import unittest

import causal
import verify_publication
from verify_timing import verify_controls


SMOKE = Path(__file__).parent / 'data/20261102/publish-smoke'


class PublicationExportTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / 'export'
        shutil.copytree(SMOKE, self.root)

    def json(self, relative):
        return json.loads((self.root / relative).read_bytes())

    def save(self, relative, value):
        (self.root / relative).write_text(json.dumps(value), encoding='utf-8')

    def mutate_trial(self, arm, function):
        cohort = self.json('publish-smoke/cohort.json')
        row = next(x for x in cohort['trials'] if x['arm'] == arm)
        function(row)
        self.save('publish-smoke/' + row['run_id'] + '/result.json',
                  {k: v for k, v in row.items() if k != 'run_id'})
        cohort['summary'] = causal.summarize(cohort['trials'], cohort['arms'], 1, cohort_valid=True)
        self.save('publish-smoke/cohort.json', cohort)

    def repack(self, omit=()):
        """Reseal mutated bytes so failures must come from semantic verification."""
        manifest = self.json('export-manifest.json')
        rows = []
        for entry in manifest['files']:
            if entry['path'] in omit:
                continue
            raw = (self.root / entry['path']).read_bytes()
            rows.append(dict(path=entry['path'], bytes=len(raw),
                             sha256=hashlib.sha256(raw).hexdigest(),
                             base64=base64.b64encode(raw).decode()))
        raw = gzip.compress(json.dumps(dict(cohort='publish-smoke', files=rows)).encode())
        (self.root / 'exact-evidence.json.gz').write_bytes(raw)
        manifest['archive'].update(bytes=len(raw), sha256=hashlib.sha256(raw).hexdigest(), files=len(rows))
        manifest['files'] = [{k: v for k, v in e.items() if k != 'base64'} for e in rows]
        self.save('export-manifest.json', manifest)

    def test_empty_archive_cannot_borrow_unarchived_files(self):
        self.repack(omit=[row['path'] for row in self.json('export-manifest.json')['files']])
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')

    def test_omitted_consumed_record_is_rejected(self):
        self.repack(omit=['publish-smoke/block-01-C/diff.resources'])
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')

    def test_zero_inventory_cannot_contradict_push_counters(self):
        def zero(row):
            snapshot = row['after_b']
            snapshot['objects'] = []
            snapshot['kinds'] = {k: dict(bytes=0, objects=0) for k in snapshot['kinds']}
            for key in ['pack_bytes','metadata_bytes','finalized_bytes','tmp_bytes']:
                snapshot[key] = 0
            row['b_object_delta'] = {k: -row['after_a'][k] for k in row['b_object_delta']}
        self.mutate_trial('F', zero)
        self.repack()
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')

    def test_changed_memory_locale_or_shared_scratch_is_rejected(self):
        def change(row):
            row['push_b']['environment'].update(GOMEMLIMIT='1GiB', LC_ALL='ko_KR.UTF-8',
                                                TMPDIR='/other-shared-scratch', TZ='Asia/Seoul')
            self.save('publish-smoke/'+row['run_id']+'/push-b.json', row['push_b'])
        self.mutate_trial('F', change)
        self.repack()
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')

    def test_exact_real_smoke_is_one_per_arm(self):
        result = verify_publication.verify(self.root, 'publish-smoke')
        self.assertEqual(result['n_per_arm'], 1)
        self.assertEqual(result['trials'], 5)
        self.assertFalse(result['independent_payload_rehash'])

    def test_publication_jobs_are_cpu_only_terminal_and_uid_cleaned(self):
        report = verify_controls(SMOKE.parent/'control',
                                 ['cdx-resident-cpu-publish-smoke','cdx-resident-cpu-publish-main'])
        self.assertEqual(report['cpu_jobs_uid_cleaned'], 2)

    def test_only_archived_source_executes_even_with_matching_cached_bytecode(self):
        source = self.root/'publish-smoke/causal.py'
        stat = source.stat()
        code = compile(source.read_bytes()+b'\nimport verify_publication; verify_publication.UNARCHIVED_BYTECODE_EXECUTED=True\n', str(source), 'exec')
        cached = Path(importlib.util.cache_from_source(str(source)))
        cached.parent.mkdir(exist_ok=True)
        header = importlib.util.MAGIC_NUMBER + struct.pack('<III', 0, int(stat.st_mtime), stat.st_size)
        cached.write_bytes(header+marshal.dumps(code))
        try:
            verify_publication.verify(self.root, 'publish-smoke')
            self.assertFalse(getattr(verify_publication, 'UNARCHIVED_BYTECODE_EXECUTED', False))
        finally:
            if hasattr(verify_publication, 'UNARCHIVED_BYTECODE_EXECUTED'):
                del verify_publication.UNARCHIVED_BYTECODE_EXECUTED

    def test_smoke_cannot_be_presented_as_main(self):
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-main')

    def test_empty_successful_cohort_is_rejected(self):
        cohort = self.json('publish-smoke/cohort.json')
        cohort['trials'] = []
        self.save('publish-smoke/cohort.json', cohort)
        self.repack()
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')

    def test_frame_treatment_cannot_be_relabelled(self):
        self.mutate_trial('TF', lambda row: row['stored_config'].update(frame=0))
        self.repack()
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')

    def test_family_clock_is_recomputed(self):
        self.mutate_trial('T', lambda row: row.update(family_publication_s=row['family_publication_s'] + 1))
        self.repack()
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')

    def test_object_inventory_must_explain_reported_bytes(self):
        self.mutate_trial('F', lambda row: row['after_b']['objects'][0].update(bytes=1))
        self.repack()
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')

    def test_roundtrip_must_use_published_identity(self):
        relative = 'publish-smoke/block-01-TA/verify-b.stdout'
        output = self.json(relative)
        output['manifest'] = 'sha256:' + '0' * 64
        self.save(relative, output)
        self.repack()
        with self.assertRaises(AssertionError):
            verify_publication.verify(self.root, 'publish-smoke')


if __name__ == '__main__':
    unittest.main()
