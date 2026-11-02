"""Replay actual retained records, never launch model or remote work in CI."""
import hashlib
import json
from pathlib import Path
from types import ModuleType
import unittest

HERE = Path(__file__).resolve().parent
DATA = HERE / 'data/20261103-three-arm'
VERIFIER_SHA = '1a1dd449b415b368761ed4fa7133f471ec6e1a141819561a937388cdaa24ff73'
TIMING_SHA = 'cc4d9b41ff92787d28c1790a71cdee1ed786765fdc5e64a9c53e2bb2e40139e8'


def load(path):
    return json.loads(path.read_bytes())


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


class RetainedThreeArmTests(unittest.TestCase):
    def test_exact_retained_receipts_and_source(self):
        manifest = load(DATA / 'manifest.json')
        self.assertEqual(manifest['n_per_arm'], 5)
        self.assertEqual(manifest['measured_source'], '1e99504a26c209a2dfba19d23c906b1899415ce3')
        self.assertEqual(manifest['evidence_verifier_source'], '1e3c74f3c61f8791eadbec704b17df10feb55241')
        rows = manifest['files']
        self.assertEqual(len(rows), 41)
        self.assertEqual(len({row['path'] for row in rows}), len(rows))
        for row in rows:
            path = (DATA / row['path']).resolve()
            self.assertTrue(path.is_relative_to(DATA.resolve()))
            raw = path.read_bytes()
            self.assertEqual(len(raw), row['bytes'], row['path'])
            self.assertEqual(sha(raw), row['sha256'], row['path'])
        for name, expected in [('three_arm_evidence.py', VERIFIER_SHA), ('verify_timing.py', TIMING_SHA)]:
            self.assertEqual(sha((HERE / name).read_bytes()), expected)

    def test_actual_fixture_smoke_and_main_replay_full_reports(self):
        raw = (HERE / 'three_arm_evidence.py').read_bytes()
        self.assertEqual(sha(raw), VERIFIER_SHA)
        self.assertEqual(sha((HERE / 'verify_timing.py').read_bytes()), TIMING_SHA)
        verifier = ModuleType('retained_checked_verifier')
        verifier.__file__ = str(HERE / 'three_arm_evidence.py')
        exec(compile(raw, verifier.__file__, 'exec'), verifier.__dict__)
        pinned = {
            'fixture': '61c1b865e1db26f8851da80914e55ad625d48b32fca2864353ce131c7a8a4de0',
            'smoke': '7618241b73110841cba1c2178285817e137f78be0a0ee9fae5096b18df7afcb1',
            'main': '60636614bf68d6653328d88e2deba59694ca163bbfaff032e64d22b48f3d643b',
        }
        for name in ['fixture', 'smoke', 'main']:
            with self.subTest(name=name):
                fixture = name == 'fixture'
                report = verifier.verify(DATA / name, 'smoke' if fixture else name, fixture=fixture,
                                         smoke_root=DATA / 'smoke' if name == 'main' else None)
                self.assertEqual(report, load(DATA / name / 'verification.stdout'))
                self.assertEqual(report['archive_sha256'], pinned[name])
                self.assertEqual(report['model_evidence'], not fixture)
                if name == 'main':
                    self.assertEqual((report['trials'], report['n_per_arm']), (15, 5))
                    self.assertIsNone(report['logical_io'])
                    self.assertIsNone(report['physical_io'])

    def test_actual_cpu_job_pod_ownership_resources_exit_and_cleanup(self):
        # Pinned actual IDs, not new Jobs or replaceable current-state queries.
        jobs = {
            'fixture': ('1cab10fc-bf2c-4430-a364-3892ec78a088', '348d37e6-ed63-4a81-9362-934fd2dfa1d7'),
            'smoke': ('d07e974d-0f2c-4fd1-926c-86fefdf540b4', 'c0d7f843-d84a-4345-b893-0c5fa4171502'),
            'main': ('6b882249-9a12-4243-a9a8-66b297f46f11', '49dc5fb1-b529-40fd-8500-e91ae947e57b'),
            'green': ('a1537e8e-3488-4ba8-9973-c58f1cc04449', '521020af-c65a-4f7f-8e6c-a47d4190e651'),
        }
        for name, (job_uid, pod_uid) in jobs.items():
            with self.subTest(name=name):
                prefix = 'cdx-resident-trio88-' + name
                control = DATA / 'control'
                create = load(control / (prefix + '-create.json'))
                cleanup = load(control / (prefix + '-cleanup.json'))
                suffix = '-latest-status.json' if name == 'green' else '-status.json'
                status = load(control / (prefix + suffix))
                self.assertEqual(create['job_uid'], job_uid)
                self.assertEqual(create['namespace_uid'], '610c63b2-f6d7-42f2-80e7-c725b2dbeb1b')
                self.assertEqual(status['job']['metadata']['uid'], job_uid)
                self.assertEqual(status['job']['status']['succeeded'], 1)
                self.assertEqual(len(status['pods']), 1)
                pod = status['pods'][0]
                self.assertEqual(pod['metadata']['uid'], pod_uid)
                owners = pod['metadata']['ownerReferences']
                self.assertTrue(any(o['uid'] == job_uid and o['kind'] == 'Job' and o['controller'] is True for o in owners))
                self.assertEqual(pod['status']['phase'], 'Succeeded')
                for state in pod['status']['containerStatuses']:
                    self.assertEqual(state['state']['terminated']['exitCode'], 0)
                    self.assertEqual(state['restartCount'], 0)
                expected = {'cpu': '8', 'memory': '16Gi'} if name == 'green' else {'cpu': '12', 'memory': '32Gi'}
                self.assertEqual(pod['spec']['containers'][0]['resources']['limits'], expected)
                self.assertFalse(pod['spec']['automountServiceAccountToken'])
                self.assertEqual(cleanup['job_uid'], job_uid)
                self.assertTrue(cleanup['job_absent'] and cleanup['pods_absent'])
                self.assertEqual({o['uid'] for o in cleanup['observations']}, {pod_uid})

    def test_retained_build_and_caller_identity(self):
        build = DATA / 'build'
        for row in load(build / 'manifest.json'):
            raw = (build / row['path']).read_bytes()
            self.assertEqual(len(raw), row['bytes'])
            self.assertEqual(sha(raw), row['sha256'])
        self.assertEqual((build / 'status.txt').read_text().strip(), 'completed')
        self.assertEqual((build / 'test-exit.txt').read_text().strip(), '0')
        self.assertIn('go1.25.1 linux/amd64', (build / 'go-version.txt').read_text())
        self.assertEqual((build / 'binary-sha256.txt').read_text().split()[0],
                         '4613b8936144f4a13e22e9e47a4df700a73062e44615a6438741bd3d14634a53')
        self.assertIn('PASS', (build / 'tests.log').read_text())
        self.assertEqual((build / 'gofmt.diff').read_bytes(), b'')
        stage = load(DATA / 'control/trio88-green-stage.json')
        self.assertEqual(stage['verified_core_source'], '05c97605b07882f9ceaaeb90b94878f8923f81cf')
        self.assertEqual(stage['core_files'], 67)
        for row in stage['files']:
            self.assertEqual(sha((HERE / row['name']).read_bytes()), row['sha256'])


if __name__ == '__main__':
    unittest.main()
