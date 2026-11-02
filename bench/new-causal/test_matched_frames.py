"""Own-format gates and real CLI fixture; never synthetic model performance."""
import copy
import json
import os
from pathlib import Path
import shutil
import struct
import sys
import tempfile
import unittest
import matched_frames as m


class MatchedFramesTests(unittest.TestCase):
    def cfg(self):
        return dict(onggi='/pinned/onggi',source_dir='/immutable',workers=8,gomaxprocs=32)

    def config(self,arm):
        fine=dict(Min=1024,Avg=4096,Max=16384)
        c=dict(schema='onggi.store.v1',chunker=dict(Min=16384,Avg=65536,Max=262144) if arm=='H' else fine,
            pack_size=64<<20)
        if arm!='D':c.update(sub_chunker=dict(Min=65536,Avg=262144,Max=1048576) if arm in ['I','F'] else fine,
            sub_kinds=['atom-prefill','atom-decode','atom-other'])
        if arm in ['F','H']:c['frame']=256<<10
        return c

    def test_policies_supply_real_framing_opportunity_and_adapter_controls(self):
        for arm in ['D','I','F','H']:
            a=m.push_argv(self.cfg(),arm,Path('/owned/trial'))
            self.assertEqual(a[a.index('--compress')+1],'zstd')
            self.assertEqual(a[a.index('--frame')+1],'256KiB' if arm in ['F','H'] else '0')
            self.assertEqual(a[a.index('--sub')+1],dict(D='0',I='256KiB',F='256KiB',H='4KiB')[arm])
            self.assertTrue(m.check_config(self.config(arm),arm))
        with self.assertRaises(ValueError):m.push_argv(self.cfg(),'unknown',Path('/owned/trial'))
        for mutate in [lambda c:c.pop('frame'),lambda c:c.update(sub_chunker=None),
                       lambda c:c.update(sub_chunker=dict(Min=1024,Avg=4096,Max=16384)),
                       lambda c:c.update(sub_kinds=['other'])]:
            c=self.config('F');mutate(c)
            with self.assertRaises(ValueError):m.check_config(c,'F')

    def test_fixed_cohort_order_and_no_partial_summary(self):
        self.assertEqual(m.orders(1),[['D','I','F','H']])
        self.assertEqual(m.orders(5),[list(s) for s in ['DIFH','IFHD','FHDI','HDIF','DIFH']])
        rows=[dict(arm=a,valid=True,status='succeeded',push=dict(elapsed_s=1,resources=dict(user_s=.2,system_s=.1,max_rss_kib=100)),
            totals=dict(pack_bytes=8,metadata_bytes=2,finalized_bytes=10)) for a in m.ARMS]
        self.assertTrue(all(v['valid'] for v in m.summarize(rows,1).values()))
        self.assertFalse(any(v['valid'] for v in m.summarize(rows,1,cohort_valid=False).values()))
        self.assertFalse(any(v['valid'] for v in m.summarize(rows[:-1],1).values()))
        rows[0]['valid']=False
        self.assertFalse(any(v['valid'] for v in m.summarize(rows,1).values()))

    def test_actual_index_frames_not_just_a_config_flag(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);(root/'index/aa').mkdir(parents=True)
            p=root/'index/aa/object'
            entry=b'a'*32+struct.pack('<IIIB3xII',0,40,8,1,0,16)
            p.write_bytes(b'ONGGIIX3'+b'b'*32+struct.pack('<I',2)+entry+entry)
            got=m.index_witness(root)
            self.assertEqual(got['stored_entries'],2);self.assertEqual(got['framed_entries'],2)
            self.assertEqual(got['composites'],0)
            p.write_bytes(p.read_bytes()[:-1])
            with self.assertRaises(ValueError):m.index_witness(root)
            p.write_bytes(b'UNKNOWN!'+b'0'*40)
            with self.assertRaises(ValueError):m.index_witness(root)

    def test_full_set_identity_and_complete_predeclared_inventory_required(self):
        base=dict(dataset_kind='model',input_inventory=[dict(path='f',bytes=1,sha256='a'*64)])
        with self.assertRaises(ValueError):m.check_dataset(base)
        base['dataset_kind']='synthetic'
        self.assertTrue(m.check_dataset(base))
        base['input_inventory'][0]['path']='../escape'
        with self.assertRaises(ValueError):m.check_dataset(base)

    def test_freeze_binds_predeclared_inventory_binary_sources_and_output_boundary(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);src=root/'inputs';src.mkdir();(src/'f').write_bytes(b'opaque fixture')
            binary=root/'onggi';binary.write_bytes(b'not an executed binary; freeze identity test')
            cfg=dict(status='not_run',dataset_kind='synthetic',source_dir=str(src),
                input_inventory=[dict(path='f',bytes=14,sha256=m.h.sha(src/'f'))],
                onggi=str(binary),binary_sha256=m.h.sha(binary),core_revision='a'*40,
                binary_build_provenance='synthetic freeze test',limits_description='local test; no run',
                workers=8,gomaxprocs=32,free_margin_bytes=1024)
            p=root/'config.json';p.write_text(json.dumps(cfg))
            frozen=m.freeze(p,root/'frozen.json');self.assertTrue(m.check_frozen(frozen))
            for output in [src,src/'nested',root]:
                with self.assertRaises(ValueError):m.guard_output(output,frozen)
            (src/'f').write_bytes(b'changed fixture')
            with self.assertRaises(ValueError):m.check_frozen(frozen)
            with self.assertRaises(ValueError):m.freeze(p,root/'other.json')

    def test_smoke_requires_actual_complete_treatments_and_frozen_identity(self):
        rows=[dict(arm=a,run_id='block-01-'+a,status='succeeded',valid=True,
            target_hash_equal=True,source_hash_equal=True,config_valid=True,flat_lists_equal=True,
            push=dict(elapsed_s=1,resources=dict(user_s=.2,system_s=.1,max_rss_kib=100)),
            totals=dict(pack_bytes=8,metadata_bytes=2,finalized_bytes=10)) for a in m.ARMS]
        smoke=dict(stage='smoke',status='succeeded',valid=True,arms=m.ARMS,frozen_sha256='a'*64,
            order=m.orders(1),trials=rows,summary=m.summarize(rows,1))
        self.assertTrue(m.check_smoke(smoke,'a'*64))
        for mutate in [lambda c:c.update(trials=[]),lambda c:c['trials'][0].update(flat_lists_equal=False),
                       lambda c:c['trials'].reverse(),lambda c:c.update(frozen_sha256='b'*64),
                       lambda c:c['summary']['F'].update(valid=False)]:
            c=copy.deepcopy(smoke);mutate(c)
            with self.assertRaises(ValueError):m.check_smoke(c,'a'*64)

    @unittest.skipUnless(sys.platform=='linux' and os.environ.get('ONGGI_TEST_BIN') and shutil.which('nice'),
                         'Actual Linux Onggi CLI required')
    def test_real_multifile_fixture_preserves_flat_lists_and_frames(self):
        with tempfile.TemporaryDirectory() as tmp:
            result=m.fixture(Path(os.environ['ONGGI_TEST_BIN']).resolve(),Path(tmp))
            self.assertTrue(result['valid'],json.dumps(result))
            self.assertEqual(len(result['trials']),4)
            by={r['arm']:r for r in result['trials']}
            self.assertEqual(by['D']['file_lists'],by['I']['file_lists'])
            self.assertEqual(by['D']['file_lists'],by['F']['file_lists'])
            for a in ['D','I','F']:self.assertEqual(by[a]['index']['composites'],0)
            for a in ['F','H']:self.assertGreater(by[a]['index']['framed_entries'],0)


if __name__=='__main__':unittest.main()
