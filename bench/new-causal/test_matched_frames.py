"""Own-format gates and real CLI fixture; never synthetic model performance."""
import copy
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
import matched_frames as m
reference=m.reference


class MatchedFramesTests(unittest.TestCase):
    def test_reference_patch_is_one_narrow_identity_split_exception(self):
        before=b'prefix\n\t\t\tif c.Sub.Max > p.Min {\nbody\n'
        after=reference.patch_store_source(before)
        self.assertIn(b'c.Sub.Max > p.Min && c.Sub.Min < p.Max',after)
        self.assertNotEqual(before,after)
        with self.assertRaises(ValueError):reference.patch_store_source(b'unexpected source')
        with self.assertRaises(ValueError):reference.patch_store_source(before+before)

    def test_changed_generator_is_rejected_before_its_code_executes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);source=Path(__file__).parent
            for name in ['matched_frames.py','causal.py','flat_frame_reference.py']:
                shutil.copyfile(source/name,root/name)
            with (root/'flat_frame_reference.py').open('a') as stream:
                stream.write('\nraise RuntimeError("MUTATED_REFERENCE_EXECUTED")\n')
            actual=subprocess.run([sys.executable,'-B',str(root/'matched_frames.py'),'--help'],capture_output=True,text=True)
            self.assertNotEqual(actual.returncode,0)
            self.assertNotIn('MUTATED_REFERENCE_EXECUTED',actual.stderr)
            self.assertIn('Unexpected reference generator source',actual.stderr)

    @unittest.skipUnless(shutil.which('git'),'Git source checkout required')
    def test_reference_generation_preserves_product_and_changes_one_guard(self):
        repo=Path(__file__).resolve().parents[2]
        product=repo/'pkg/store/store.go';before=product.read_bytes()
        with tempfile.TemporaryDirectory() as tmp:
            result=reference.generate(repo,Path(tmp)/'reference')
            self.assertEqual(result['modified'],['pkg/store/store.go'])
            self.assertFalse(result['product_source_edited'])
            self.assertEqual(product.read_bytes(),before)
            self.assertTrue((Path(result['reference_source'])/'go.mod').is_file())
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

    def test_marker_only_reference_receipt_never_admits_binary(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);source=root/'input';source.mkdir();(source/'f').write_bytes(b'opaque')
            binary=root/'not-executed';binary.write_bytes(b'synthetic non-executed binary')
            proof=root/'reference-manifest.json'
            proof.write_text(json.dumps(dict(schema='onggi.flat-frame-reference.v1',status='built',
                binary_sha256=m.h.sha(binary),modified=['pkg/store/store.go'])))
            cfg=dict(status='not_run',dataset_kind='synthetic',source_dir=str(source),input_inventory=m.h.inventory(source),
                onggi=str(binary),binary_sha256=m.h.sha(binary),core_revision='a'*40,binary_build_provenance='synthetic',
                limits_description='synthetic',workers=8,gomaxprocs=32,free_margin_bytes=1024,reference_manifest_path=str(proof))
            path=root/'config.json';path.write_text(json.dumps(cfg))
            with self.assertRaises(ValueError):m.freeze(path,root/'frozen.json')

    @unittest.skipUnless(shutil.which('git'),'Git source checkout required')
    def test_reference_proof_parser_binds_sources_patch_commands_and_raw_logs(self):
        # Synthetic parser records only; actual execution is the separate Linux fixture.
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)/'reference';repo=Path(__file__).resolve().parents[2]
            record=reference.generate(repo,root);binary=str(root/'onggi')
            record.update(status='built',binary=binary,binary_sha256='a'*64,commands=[])
            commands=[['go','version'],['go','vet','./...'],['go','test','-race','./...'],
                ['go','build','-buildvcs=false','-trimpath','-ldflags','-s -w','-o',binary,'./cmd/onggi']]
            for i,argv in enumerate(commands):
                stdout={0:b'go version go0 synthetic\n',2:b'ok\tgithub.com/ai-computing/onggi/pkg/store\tsynthetic\n'}.get(i,b'')
                (root/('command-%d.stdout'%i)).write_bytes(stdout);(root/('command-%d.stderr'%i)).write_bytes(b'')
                record['commands'].append(dict(argv=argv,exit_code=0,stdout_sha256=reference.digest(stdout),stderr_sha256=reference.digest(b'')))
            p=root/'reference-manifest.json';p.write_text(json.dumps(record))
            self.assertEqual(reference.validate_proof(p,'a'*64)['status'],'built')
            for mutate in [lambda r:r.update(base_revision='b'*40),lambda r:r.update(generator_sha256='b'*64),
                lambda r:r.pop('patch'),lambda r:r['files'].pop(),lambda r:r['commands'][2].update(exit_code=1),
                lambda r:r['commands'][2].update(stdout_sha256='b'*64)]:
                changed=copy.deepcopy(record);mutate(changed);p.write_text(json.dumps(changed))
                with self.assertRaises(ValueError):reference.validate_proof(p,'a'*64)
            p.write_text(json.dumps(record));(root/'command-2.stdout').write_bytes(b'changed log')
            with self.assertRaises(ValueError):reference.validate_proof(p,'a'*64)

    def test_smoke_requires_actual_complete_treatments_and_frozen_identity(self):
        cfg=self.cfg();cfg.update(gomemlimit=None,input_inventory=[dict(path='f',bytes=1,sha256='a'*64)])
        rows=[dict(arm=a,run_id='block-01-'+a,status='succeeded',valid=True,
            target_hash_equal=True,source_hash_equal=True,config_valid=True,flat_lists_equal=True,
            push=dict(elapsed_s=1,resources=dict(user_s=.2,system_s=.1,max_rss_kib=100)),
            totals=dict(pack_bytes=8,metadata_bytes=2,finalized_bytes=10)) for a in m.ARMS]
        smoke=dict(stage='smoke',status='succeeded',valid=True,arms=m.ARMS,frozen_sha256='a'*64,
            order=m.orders(1),trials=rows,summary=m.summarize(rows,1))
        # The previous marker-only smoke must be rejected, despite consistent summaries.
        with self.assertRaises(ValueError):m.check_smoke(smoke,'a'*64,cfg)
        for row in rows:
            arm=row['arm'];root=Path('/owned'/Path(row['run_id']))
            manifest=dict(schema='onggi.manifest.v1',bytes=1,files=[dict(path='f',size=1,list='sha256:'+'b'*64)])
            raw=json.dumps(manifest).encode();md='sha256:'+hashlib.sha256(raw).hexdigest()
            row.update(trial_root=str(root),stored_config=self.config(arm),file_lists={'f':'sha256:'+'b'*64},
                target_inventory=cfg['input_inventory'],source_inventory_after=cfg['input_inventory'],
                index=dict(stored_entries=1,framed_entries=0,composites=0,objects=[]),
                cli=dict(manifest=md,stats=dict(files=1,bytes=1,skipped=None)),
                manifest_witness=dict(sha256=md[7:],base64=base64.b64encode(raw).decode()))
            env=dict(GOMAXPROCS='32',GOMEMLIMIT=None,LC_ALL='C',TZ='UTC',TMPDIR=str(root/'tmp'))
            row['push'].update(argv=['nice','-n','19']+m.push_argv(cfg,arm,root),status='succeeded',exit_code=0,environment=env)
            row['pull']=dict(argv=['nice','-n','19',cfg['onggi'],'pull','--store',str(root/'store'),'--workers','8',
                '--json',md,str(root/'output')],status='succeeded',exit_code=0,environment=env,elapsed_s=1)
        smoke['summary']=m.summarize(rows,1)
        self.assertTrue(m.check_smoke(smoke,'a'*64,cfg))
        for mutate in [lambda c:c.update(trials=[]),lambda c:c['trials'][0].update(flat_lists_equal=False),
                       lambda c:c['trials'].reverse(),lambda c:c.update(frozen_sha256='b'*64),
                       lambda c:c['summary']['F'].update(valid=False),
                       lambda c:c['trials'][2].pop('stored_config'),
                       lambda c:c['trials'][2]['index'].update(composites=1),
                       lambda c:c['trials'][1].update(target_inventory=[]),
                       lambda c:c['trials'][2]['file_lists'].update(f='different-list')]:
            c=copy.deepcopy(smoke);mutate(c)
            with self.assertRaises(ValueError):m.check_smoke(c,'a'*64,cfg)

    @unittest.skipUnless(sys.platform=='linux' and os.environ.get('ONGGI_TEST_BIN') and shutil.which('nice'),
                         'Actual Linux Onggi CLI required')
    def test_unmodified_product_explicitly_refuses_identity_adapter(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);source=root/'input';source.mkdir();(source/'f.rbln').write_bytes(b'opaque test')
            cfg=self.cfg();cfg.update(onggi=str(Path(os.environ['ONGGI_TEST_BIN']).resolve()),source_dir=str(source))
            (root/'tmp').mkdir()
            actual=subprocess.run(m.push_argv(cfg,'I',root),capture_output=True,text=True)
            self.assertNotEqual(actual.returncode,0)
            self.assertIn('sub-chunk max 1048576 exceeds chunk min 1024',actual.stderr)
            print('Expected unmodified-product refusal: '+actual.stderr.strip())

    @unittest.skipUnless(sys.platform=='linux' and os.environ.get('ONGGI_TEST_BIN') and shutil.which('nice'),
                         'Actual Linux Onggi CLI required')
    def test_real_multifile_fixture_preserves_flat_lists_and_frames(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo=Path(__file__).resolve().parents[2]
            built=reference.build(repo,root/'reference')
            fixture=root/'fixture';fixture.mkdir()
            result=m.fixture(Path(built['binary']),fixture,root/'reference/reference-manifest.json')
            self.assertTrue(result['valid'],json.dumps(result))
            self.assertEqual(len(result['trials']),4)
            by={r['arm']:r for r in result['trials']}
            self.assertEqual(by['D']['file_lists'],by['I']['file_lists'])
            self.assertEqual(by['D']['file_lists'],by['F']['file_lists'])
            for a in ['D','I','F']:self.assertEqual(by[a]['index']['composites'],0)
            for a in ['F','H']:self.assertGreater(by[a]['index']['framed_entries'],0)


if __name__=='__main__':unittest.main()
