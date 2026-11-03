import hashlib
import copy
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import unittest

import cohort as c
import diff_keep


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


class CohortTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def fixture(self):
        for name in ['base', 'target', 'store', 'cache']:
            (self.root/name).mkdir()
        base, target = self.root/'base/base.bin', self.root/'target/target.bin'
        base.write_bytes(b'AAAABBBBCCCC')
        target.write_bytes(b'AAAAxxxxCCCC')
        (self.root/'store/record').write_bytes(b'immutable store fixture')
        (self.root/'cache/record').write_bytes(b'immutable cache fixture')
        for name in ['store','cache']:
            for relative in ['tmp','refs','config/st']:
                (self.root/name/relative).mkdir(parents=True,exist_ok=True)
            (self.root/name/'config/st/store').write_text('{}',encoding='utf-8')
        diff = self.root/'diff.json'
        diff.write_text(json.dumps(dict(files=[dict(path='target.bin', base='base.bin', size=12,
            same=8, moved=0, new=4, changed_runs=1, largest_run=4, changed=[[4,4]])])), encoding='utf-8')
        prepared = diff_keep.prepare(base, target, diff, self.root/'patch',
            diff_sha256=sha(diff), base_path='base.bin', target_path='target.bin',
            base_sha256=sha(base), target_sha256=sha(target))
        receipt = self.root/'build.json'
        receipt.write_text('{}', encoding='utf-8')
        driver = self.root/'intentionally-invalid-driver'
        driver.write_bytes(b'not an executable; negative command fixture')
        cfg = dict(status='not_run', dataset_kind='fixture', workers=8, gomaxprocs=32,
            base_dir=str(self.root/'base'), target_dir=str(self.root/'target'),
            store=str(self.root/'store'), cache_dir=str(self.root/'cache'), patch_dir=str(self.root/'patch'),
            patch_manifest_sha256=prepared['patch']['manifest_sha256'],
            patch_preparation_sha256=sha(self.root/'patch/preparation.json'),
            driver=str(driver), driver_sha256=sha(driver),
            base_manifest='sha256:'+'a'*64, target_manifest='sha256:'+'b'*64,
            base_inventory=[dict(path='base.bin', bytes=12, sha256=sha(base))],
            target_inventory=[dict(path='target.bin', bytes=12, sha256=sha(target))],
            source_revision='1'*40, build_receipts=[dict(path=str(receipt), sha256=sha(receipt))],
            limits_description='real temporary file fixture; no model performance',
            cleanup_success_data=False, command_timeout_seconds=30, free_margin_bytes=1)
        config = self.root/'config.json'
        config.write_text(json.dumps(cfg), encoding='utf-8')
        return c.freeze(config, self.root/'frozen.json')

    def test_preregistered_order_is_not_selected_from_results(self):
        self.assertEqual(c.orders(1), [['R', 'L']])
        self.assertEqual(c.orders(5), [['R','L'], ['L','R'], ['R','L'], ['L','R'], ['R','L']])
        with self.assertRaises(ValueError):
            c.orders(4)

    def test_real_literal_trial_has_common_endpoint_and_intact_sources(self):
        cfg = self.fixture()
        row = c.trial(cfg, 'L', self.root/'literal')
        self.assertTrue(row['valid'], row)
        self.assertEqual((self.root/'literal/ready/target.bin').read_bytes(), b'AAAAxxxxCCCC')
        self.assertFalse((self.root/'literal/base/base.bin').exists())
        self.assertEqual((self.root/'base/base.bin').read_bytes(), b'AAAABBBBCCCC')
        self.assertEqual(row['target_sha256'], cfg['target_inventory'][0]['sha256'])
        self.assertEqual(row['ready_seconds'], (row['published_ns']-row['online_start_ns'])/1e9)
        self.assertGreaterEqual(row['audit_start_ns'], row['published_ns'])
        self.assertGreaterEqual(row['preparation_inclusive_seconds'], row['ready_seconds'])

    def test_changed_inputs_and_wrong_patch_pin_refused(self):
        cfg = self.fixture()
        (self.root/'target/target.bin').write_bytes(b'bad')
        with self.assertRaises(ValueError):
            c.check_frozen(cfg)
        (self.root/'target/target.bin').write_bytes(b'AAAAxxxxCCCC')
        cfg['patch_manifest_sha256'] = '0'*64
        with self.assertRaises(ValueError):
            c.check_frozen(cfg)

    def test_failed_driver_retains_work_and_never_publishes(self):
        cfg = self.fixture()
        row = c.trial(cfg, 'R', self.root/'bad-driver')
        self.assertFalse(row['valid'])
        self.assertEqual(row['status'], 'failed')
        self.assertTrue((self.root/'bad-driver/base/base.bin').is_file())
        self.assertFalse((self.root/'bad-driver/ready/target.bin').exists())
        self.assertTrue((self.root/'bad-driver/deploy.stderr').is_file())

    def test_missing_or_forged_smoke_cannot_start_main(self):
        self.fixture()
        with self.assertRaises(ValueError):
            c.run(self.root/'frozen.json', self.root/'main', 'main')
        self.assertFalse((self.root/'main').exists())
        smoke = self.root/'fake-smoke.json'
        smoke.write_text(json.dumps(dict(valid=True, status='succeeded', stage='smoke')), encoding='utf-8')
        with self.assertRaises(ValueError):
            c.run(self.root/'frozen.json', self.root/'main', 'main', smoke_path=smoke, smoke_sha256=sha(smoke))
        self.assertFalse((self.root/'main').exists())

    def test_overlap_and_existing_output_refused(self):
        self.fixture()
        with self.assertRaises(ValueError):
            c.run(self.root/'frozen.json', self.root/'base/overwrite', 'smoke')
        existing = self.root/'already'
        existing.mkdir()
        (existing/'sentinel').write_bytes(b'keep')
        with self.assertRaises(FileExistsError):
            c.run(self.root/'frozen.json', existing, 'smoke')
        self.assertEqual((existing/'sentinel').read_bytes(), b'keep')

    def test_only_successful_owned_data_cleanup(self):
        cfg = self.fixture()
        cfg['cleanup_success_data'] = True
        row = c.trial(cfg, 'L', self.root/'clean')
        self.assertTrue(row['valid'], row)
        self.assertFalse((self.root/'clean/ready').exists())
        self.assertTrue((self.root/'clean/result.json').is_file())
        self.assertTrue((self.root/'clean/deploy.stdout').is_file())
        self.assertTrue((self.root/'base/base.bin').is_file())

    def test_model_mode_cannot_relabel_tiny_fixture_as_model_evidence(self):
        self.fixture()
        cfg = json.loads((self.root/'config.json').read_bytes())
        cfg['dataset_kind'] = 'model'
        config = self.root/'false-model.json'
        config.write_text(json.dumps(cfg), encoding='utf-8')
        with self.assertRaises(ValueError):
            c.freeze(config, self.root/'model-frozen.json')

    def test_smoke_rechecks_treatment_instead_of_trusting_valid_flag(self):
        cfg = self.fixture()
        literal = c.trial(cfg, 'L', self.root/'literal')
        self.assertTrue(literal['valid'], literal)
        fake_r = copy.deepcopy(literal)
        fake_r['arm'] = 'R'
        rows = [fake_r,literal]
        report = dict(stage='smoke', frozen_sha256='f'*64, dataset_kind='fixture',
                      status='succeeded',valid=True,order=c.orders(1),trials=rows,summary=c.summarize(rows))
        with self.assertRaises(ValueError):
            c.smoke_admission(report,'f'*64,cfg)

    def test_timeout_is_failure_with_retained_stderr_and_exit_receipt(self):
        directory = self.root/'timeout'
        directory.mkdir()
        (directory/'tmp').mkdir()
        row = c.execute(dict(command_timeout_seconds=1),
                        [sys.executable,'-c','import time; time.sleep(10)'],directory)
        self.assertEqual(row['status'],'failed')
        self.assertIsNone(row['exit_code'])
        self.assertTrue((directory/'command.json').is_file())
        self.assertTrue((directory/'deploy.stderr').is_file())

    def test_ordinary_origin_refused_before_driver_can_create_directories(self):
        cfg = self.fixture()
        ordinary = self.root/'ordinary'
        ordinary.mkdir()
        (ordinary/'sentinel').write_bytes(b'keep immutable')
        cfg['store'] = str(ordinary)
        before = sorted(p.relative_to(ordinary).as_posix() for p in ordinary.rglob('*'))
        with self.assertRaises(ValueError):
            c.trial(cfg,'R',self.root/'must-not-run')
        self.assertFalse((self.root/'must-not-run').exists())
        self.assertEqual(sorted(p.relative_to(ordinary).as_posix() for p in ordinary.rglob('*')),before)
        self.assertEqual((ordinary/'sentinel').read_bytes(),b'keep immutable')
        config = self.root/'ordinary-config.json'
        config.write_text(json.dumps(cfg),encoding='utf-8')
        with self.assertRaises(ValueError):
            c.freeze(config,self.root/'ordinary-frozen.json')

    @unittest.skipUnless(os.environ.get('ONGGI_TEST_BIN') and os.environ.get('ONGGI_TEST_DRIVER'), 'Linux Go binaries required')
    def test_real_go_rl_smoke_main_and_full_aggregate(self):
        report = c.fixture(os.environ['ONGGI_TEST_BIN'], os.environ['ONGGI_TEST_DRIVER'], self.root/'integration')
        self.assertTrue(report['valid'], report)
        self.assertEqual(len(report['trials']), 10)
        self.assertEqual({k: v['n'] for k,v in report['summary'].items()}, {'R':5,'L':5})
        self.assertTrue(all(r['target_sha256'] == report['trials'][0]['target_sha256'] for r in report['trials']))


if __name__ == '__main__':
    unittest.main()
