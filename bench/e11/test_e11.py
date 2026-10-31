import io
import json
import os
import pathlib
import tempfile
import unittest
from unittest.mock import patch

from common import BodyCounter, aggregate, validate_cohort, validate_ready
from deploy import template
import origin


class EvidenceTests(unittest.TestCase):
    def test_proxy_counts_read_bytes_even_when_client_disconnects(self):
        class Connection:
            status=206
            def request(self,*a,**kw): pass
            def getresponse(self): return self
            def getheaders(self): return [('Content-Length','7')]
            def read(self,n): return b'payload'
            def close(self): pass
        class Broken:
            def write(self,b): raise BrokenPipeError('client gone')
        h=origin.Proxy.__new__(origin.Proxy)
        h.path='/e11/packs/ab/ab';h.command='GET';h.headers={};h.client_address=('client',1)
        h.wfile=Broken();h.send_response=lambda *a:None;h.send_header=lambda *a:None;h.end_headers=lambda:None
        counter=BodyCounter()
        with tempfile.TemporaryDirectory() as d, patch.object(origin,'ROOT',pathlib.Path(d)), \
                patch.object(origin.http.client,'HTTPConnection',return_value=Connection()), \
                patch.dict(origin.STATE,dict(counters=counter,trial=None,active_proxy=0)):
            h.forward()
            row=json.loads((pathlib.Path(d)/'origin-requests.jsonl').read_text())
        self.assertEqual(row['body_bytes'],7)
        self.assertEqual(row['written_body_bytes'],0)
        self.assertIn('BrokenPipeError',row['transfer_error'])
        self.assertEqual(counter.snapshot()['transfer_errors'],1)

    def test_timeout_preserves_captured_output(self):
        with patch.dict(os.environ,dict(E11_ADMIN='http://test',E11_STORE='s3://test',NODE_NAME='test')):
            import worker
        import subprocess
        with tempfile.TemporaryDirectory() as d, patch.object(worker.subprocess,'run',
                side_effect=subprocess.TimeoutExpired(['cmd'],1,output=b'out',stderr=b'err')):
            with self.assertRaises(subprocess.TimeoutExpired):
                worker.run([],pathlib.Path(d),'pull')
            self.assertEqual((pathlib.Path(d)/'pull.stdout').read_bytes(),b'out')
            self.assertEqual((pathlib.Path(d)/'pull.stderr').read_bytes(),b'err')
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
        counter = BodyCounter()
        counter.record('/e11/packs/ab/ab',3,206)
        return dict(nodes=['a', 'b', 'c'], arm='store', stratum='empty', target_manifest='target',
                    trial='main-01', stage='main', status='completed',
                    release_mono=5.0, pulled={'a': 6.0, 'b': 8.0, 'c': 7.0},
                    started={'a': 5.01, 'b': 5.02, 'c': 5.03},
                    origin=counter.snapshot(),
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
        for r in c['results'].values():
            r['manifest']='wrong-shared-target'
        with self.assertRaises(ValueError):
            validate_cohort(c)
        c = self.cohort()
        c['results']['a']['stats']['peer_bytes']=123
        with self.assertRaises(ValueError):
            validate_cohort(c)
        c = self.cohort()
        c['origin']['pack_body_bytes']=4
        with self.assertRaises(ValueError):
            validate_cohort(c)

    def test_ready_cache_placement_is_verified(self):
        cfg=dict(path='config/st/store',bytes=3,sha256='config')
        inputs=dict(store_config=[dict(path='config/st/store',base64='YWJj',sha256='config')])
        ready={n:dict(cache_before=[cfg]) for n in ['a','b','c']}
        c=dict(nodes=['a','b','c'],ready=ready,stratum='empty')
        validate_ready(c,inputs,[cfg,dict(path='packs/x',bytes=5,sha256='seed')])
        c['ready']['b']['cache_before'].append(dict(path='packs/leak',bytes=1,sha256='leak'))
        with self.assertRaises(ValueError):validate_ready(c,inputs,[])
        c['ready']['b']['cache_before']=[cfg]
        c['stratum']='one-seed'
        with self.assertRaises(ValueError):validate_ready(c,inputs,[cfg,dict(path='packs/x',bytes=5,sha256='seed')])
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
        out = aggregate(cohorts + [smoke], required_conditions={('empty','store')})
        self.assertEqual(out['groups'][0]['cohort_completion_s']['n'], 5)
        self.assertEqual(out['groups'][0]['cohort_completion_s']['median'], 3.0)
        self.assertEqual(out['groups'][0]['cohort_completion_s']['iqr'], 2.0)
        with self.assertRaises(ValueError):
            aggregate(cohorts[:4])
        with self.assertRaises(ValueError):
            aggregate(cohorts)
        cohorts[0]['results']['a']['returncode'] = 1
        with self.assertRaises(ValueError):
            aggregate(cohorts, required_conditions={('empty','store')})


if __name__ == '__main__':
    unittest.main()
