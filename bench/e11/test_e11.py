import io
import unittest

from common import BodyCounter, aggregate, validate_cohort
from deploy import template


class EvidenceTests(unittest.TestCase):
    def test_origin_uses_actual_host_owner_and_readonly_source(self):
        origin = template('cdx-test','etri-001',False,'frozen')
        spec = origin['spec']['template']['spec']
        self.assertEqual(spec['securityContext']['runAsUser'],1002)
        mounts = spec['containers'][0]['volumeMounts']
        self.assertTrue(next(m for m in mounts if m['name']=='store')['readOnly'])
        self.assertFalse(spec['automountServiceAccountToken'])
        for c in spec['containers']:
            self.assertEqual(set(c['resources']['limits']),{'cpu','memory'})
    def test_actual_successful_body_accounting_includes_metadata(self):
        c = BodyCounter()
        c.record('/e11/packs/ab/abcdef', 123, 206)
        c.record('/e11/manifests/cd/cdef', 17, 200)
        c.record('/e11/no-such-key', 9, 404)
        self.assertEqual(c.snapshot()['successful_body_bytes'], 140)
        self.assertEqual(c.snapshot()['pack_body_bytes'], 123)
        self.assertEqual(c.snapshot()['metadata_body_bytes'], 17)
        self.assertEqual(c.snapshot()['error_body_bytes'], 9)
        with self.assertRaises(ValueError):
            c.record('/e11/packs/x', -1, 200)

    def cohort(self):
        return dict(nodes=['a', 'b', 'c'], arm='store', stratum='empty',
                    trial='main-01', stage='main', status='completed',
                    release_mono=5.0, pulled={'a': 6.0, 'b': 8.0, 'c': 7.0},
                    started={'a': 5.01, 'b': 5.02, 'c': 5.03},
                    origin=BodyCounter().snapshot(),
                    results={n: dict(status='completed', hashes_match=True,
                        pull_s=1.0, returncode=0, manifest='target',
                        stats=dict(read_bytes=1, cache_bytes=0, peer_bytes=0))
                        for n in ['a', 'b', 'c']})

    def test_cohort_fails_closed_on_missing_or_bad_node(self):
        c = self.cohort()
        self.assertEqual(validate_cohort(c)['cohort_completion_s'], 3.0)
        del c['results']['c']
        with self.assertRaises(ValueError):
            validate_cohort(c)
        c = self.cohort()
        c['results']['a']['hashes_match'] = False
        with self.assertRaises(ValueError):
            validate_cohort(c)
        c = self.cohort()
        c['pulled']['a'] = 4.0
        with self.assertRaises(ValueError):
            validate_cohort(c)

    def test_aggregate_requires_n5_and_excludes_only_declared_smoke(self):
        cohorts = []
        for i in range(5):
            c = self.cohort()
            c['trial'] = 'main-' + str(i)
            c['pulled']['b'] = 6.0 + i
            cohorts.append(c)
        smoke = self.cohort()
        smoke['trial'] = 'smoke-01'
        smoke['stage'] = 'smoke'
        out = aggregate(cohorts + [smoke])
        self.assertEqual(out['groups'][0]['cohort_completion_s']['n'], 5)
        self.assertEqual(out['groups'][0]['cohort_completion_s']['median'], 3.0)
        self.assertEqual(out['groups'][0]['cohort_completion_s']['iqr'], 2.0)
        with self.assertRaises(ValueError):
            aggregate(cohorts[:4])
        cohorts[0]['results']['a']['returncode'] = 1
        with self.assertRaises(ValueError):
            aggregate(cohorts)


if __name__ == '__main__':
    unittest.main()
