"""Treatment witnesses and cohort admission, not model performance fixtures."""
import copy
import hashlib
import importlib.util
import marshal
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
import three_arm as t


class ThreeArmTests(unittest.TestCase):
    def fixture(self, arm):
        cfg = dict(base_manifest='sha256:'+'a'*64, target_manifest='sha256:'+'b'*64,
                   base_inventory=[dict(path='base.rbln', bytes=10, sha256='a'*64)],
                   target_inventory=[dict(path='target.rbln', bytes=10, sha256='b'*64)])
        report = dict(policy=arm, manifest=cfg['target_manifest'], claim_calls=0 if arm == 'P' else 1,
                      admitted=arm == 'R', stats=dict(base=cfg['base_manifest'] if arm == 'R' else None,
                        reused=9 if arm == 'R' else 0, local_base=cfg['base_manifest'] if arm == 'C' else None,
                        local_bytes=9 if arm == 'C' else 0, read_bytes=0, peer_bytes=0, cached=0))
        proof = dict(base_inventory_after=[] if arm == 'R' else copy.deepcopy(cfg['base_inventory']),
                     base_identity_before=[1, 2], base_identity_after=None if arm == 'R' else [1, 2],
                     target_identity=[1, 2] if arm == 'R' else [1, 3])
        return cfg, report, proof

    def test_fixed_order_preserves_five_p_r_alternating_pairs(self):
        expected = [['P', 'C', 'R'], ['R', 'C', 'P'], ['C', 'P', 'R'], ['R', 'P', 'C'], ['P', 'R', 'C']]
        self.assertEqual(t.orders(5), expected)
        self.assertEqual(t.orders(1), expected[:1])
        with self.assertRaises(ValueError):
            t.orders(6)

    def test_all_actual_treatment_witnesses_are_required(self):
        for arm in ['P', 'C', 'R']:
            cfg, report, proof = self.fixture(arm)
            self.assertTrue(t.validate_treatment(cfg, arm, report, proof))

    def test_c_cache_fallback_is_not_an_intact_base_copy_result(self):
        cfg, report, proof = self.fixture('C')
        report['stats'].update(local_bytes=0, local_base=None)
        with self.assertRaises(ValueError):
            t.validate_treatment(cfg, 'C', report, proof)

    def test_changed_donor_or_inode_is_rejected(self):
        for field, value in [('base_inventory_after', []), ('base_identity_after', [1, 4])]:
            cfg, report, proof = self.fixture('C')
            proof[field] = value
            with self.assertRaises(ValueError):
                t.validate_treatment(cfg, 'C', report, proof)

    def test_r_reconstruction_is_not_consumption(self):
        cfg, report, proof = self.fixture('R')
        proof['target_identity'] = [1, 99]
        with self.assertRaises(ValueError):
            t.validate_treatment(cfg, 'R', report, proof)

    def test_wrong_target_claim_or_payload_fetch_is_rejected(self):
        for arm in ['P', 'C', 'R']:
            for field in ['manifest', 'claim_calls', 'read_bytes']:
                cfg, report, proof = self.fixture(arm)
                if field == 'read_bytes':
                    report['stats'][field] = 1
                elif field == 'claim_calls':
                    report[field] += 1
                else:
                    report[field] = cfg['base_manifest']
                with self.assertRaises(ValueError):
                    t.validate_treatment(cfg, arm, report, proof)

    def test_main_requires_matching_complete_three_arm_smoke(self):
        smoke = dict(stage='smoke', status='succeeded', valid=True, traced=False,
                     frozen_sha256='f'*64, arms=['P', 'C', 'R'], order=t.orders(1), trials=[])
        for arm in t.ARMS:
            cfg, report, proof = self.fixture(arm)
            smoke['trials'].append(dict(arm=arm, run_id='block-01-'+arm, status='succeeded', valid=True,
                treatment_valid=True, output_hash_equal=True, output_inventory=cfg['target_inventory'],
                cli=report, donor_proof=proof, validated_s=2,
                command=dict(status='succeeded', exit_code=0, traced=False, elapsed_s=1)))
        smoke['summary'] = t.h.summarize(smoke['trials'], t.ARMS, 1)
        self.assertTrue(t.check_smoke('f'*64, False, smoke, cfg))
        for key, value in [('frozen_sha256', 'a'*64), ('traced', True), ('arms', ['P', 'R']),
                           ('stage', 'main'), ('valid', False), ('status', 'failed')]:
            bad = dict(smoke, **{key: value})
            with self.assertRaises(ValueError):
                t.check_smoke('f'*64, False, bad, cfg)
        mutations = [lambda x: x.update(trials=x['trials'][:2]),
                     lambda x: x.update(order=[['C', 'P', 'R']]),
                     lambda x: x['trials'][1].update(run_id='block-02-C'),
                     lambda x: x['trials'][1].update(output_inventory=[]),
                     lambda x: x['trials'][1]['cli']['stats'].update(local_bytes=0),
                     lambda x: x['trials'][1]['command'].update(exit_code=1),
                     lambda x: x['summary']['C'].update(valid=False)]
        for mutate in mutations:
            bad = copy.deepcopy(smoke)
            mutate(bad)
            with self.assertRaises(ValueError):
                t.check_smoke('f'*64, False, bad, cfg)

    def test_changed_caller_or_harness_refuses_measurement(self):
        with tempfile.TemporaryDirectory() as directory:
            driver = Path(directory) / 'driver'
            source = Path(directory) / 'caller.go'
            driver.write_bytes(b'test executable identity')
            source.write_bytes(b'test source identity')
            cfg = dict(driver=str(driver), driver_sha256=hashlib.sha256(driver.read_bytes()).hexdigest(),
                       driver_source=str(source), driver_source_sha256=hashlib.sha256(source.read_bytes()).hexdigest(),
                       three_arm_sha256=hashlib.sha256(Path(t.__file__).read_bytes()).hexdigest())
            self.assertTrue(t.check_extra_sources(cfg))
            for key in ['driver_sha256', 'driver_source_sha256', 'three_arm_sha256']:
                with self.assertRaises(ValueError):
                    t.check_extra_sources(dict(cfg, **{key: '0'*64}))

    def test_empty_smoke_cannot_admit_main(self):
        smoke = dict(stage='smoke', status='succeeded', valid=True, traced=False,
                     frozen_sha256='f'*64, arms=['P', 'C', 'R'], order=[], trials=[])
        with self.assertRaises(ValueError):
            t.check_smoke('f'*64, False, smoke)

    def test_matching_cached_bytecode_cannot_replace_helper_source(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ['causal.py', 'three_arm.py']:
                shutil.copyfile(Path(t.__file__).parent/name, root/name)
            source = root/'causal.py'
            marker = root/'executed-cached-code'
            payload = compile(source.read_bytes()+('\nopen(%r,"w").write("executed")\n' % str(marker)).encode(),
                              str(source), 'exec')
            cache = Path(importlib.util.cache_from_source(str(source)))
            cache.parent.mkdir()
            cache.write_bytes(importlib.util.MAGIC_NUMBER+struct.pack('<III', 0, int(source.stat().st_mtime),
                              source.stat().st_size)+marshal.dumps(payload))
            run = subprocess.run([sys.executable, '-B', str(root/'three_arm.py'), '--help'],
                                 capture_output=True, text=True)
            self.assertEqual(run.returncode, 0, run.stderr)
            self.assertFalse(marker.exists(), 'Timestamp-valid cached helper executed despite frozen source')

    def test_trace_requires_actual_donor_reads_for_c_and_none_for_p_r(self):
        roots = {key: '/'+key for key in ['base', 'target', 'store', 'source_base', 'source_target']}
        lines = ['pwrite64(4</target/file>, "", 10, 0) = 10', 'fsync(4</target/file>) = 0']
        groups = t.h.parse_trace(lines, roots)['groups']
        with self.assertRaises(ValueError):
            t.validate_io('C', dict(local_bytes=9), groups, 10)
        with_read = t.h.parse_trace(lines+['pread64(3</base/file>, "", 9, 0) = 9'], roots)['groups']
        self.assertTrue(t.validate_io('C', dict(local_bytes=9), with_read, 10))
        self.assertTrue(t.validate_io('P', dict(local_bytes=0), groups, 10))
        r_lines = ['pwrite64(4</target/file>, "", 1, 9) = 1', 'fsync(4</target/file>) = 0']
        self.assertTrue(t.validate_io('R', dict(local_bytes=0), t.h.parse_trace(r_lines, roots)['groups'], 10))
        for arm, writes in [('P', lines), ('R', r_lines)]:
            bad = t.h.parse_trace(writes+['pread64(3</base/file>, "", 9, 0) = 9'], roots)['groups']
            with self.assertRaises(ValueError):
                t.validate_io(arm, dict(local_bytes=0), bad, 10)


if __name__ == '__main__':
    unittest.main()
