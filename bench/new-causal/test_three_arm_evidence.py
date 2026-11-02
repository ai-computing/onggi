"""Tampered archive/clock/branch mutations; synthetic data is not model evidence."""
import copy
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import tarfile
import tempfile
import unittest

import three_arm as t
import three_arm_evidence as e


class EvidenceTests(unittest.TestCase):
    def write(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value, sort_keys=True)+'\n', encoding='utf-8')

    def records(self, root, traced=False, stage='smoke', actual_fixture=False):
        assert not actual_fixture or (traced and stage=='smoke')
        label = 'smoke' if actual_fixture else ('io-' if traced else '')+stage
        n = 1 if stage=='smoke' else 5
        cohort_root = root/label
        cohort_root.mkdir()
        for name in ['three_arm.py', 'causal.py']:
            (cohort_root/name).write_bytes(Path(t.__file__).with_name(name).read_bytes())
        cfg = dict(workers=8, gomaxprocs=32, source_revision='05c97605b07882f9ceaaeb90b94878f8923f81cf',
            driver='/runs/trio88-green/three-arm-call', driver_sha256='4613b8936144f4a13e22e9e47a4df700a73062e44615a6438741bd3d14634a53',
            driver_source_sha256='b6015c42ea40772f57d0ac89e013597ad63201ac5ba0e61f7e2bb1183e7dab81',
            three_arm_sha256='b04a44fe2928247e8b4c588e5b9a20e3fcd48c2f04b0bc9577e83a31bfe4abd9',
            harness_sha256='1546801671fc19d3871487b2d2d4f0f8e445bae82cbfd25aabf9e87351554f5a',
            store='/store', base_dir='/source-base', target_dir='/source-target', cache_dir='/seed-cache',
            base_manifest='sha256:'+'a'*64,target_manifest='sha256:'+'b'*64,
            base_inventory=[dict(path='base.rbln', bytes=10, sha256='a'*64)],
            target_inventory=[dict(path='target.rbln', bytes=10, sha256='b'*64)],
            cache_inventory=[dict(path='config/test',bytes=4,sha256='c'*64)],
            deployment_arms=t.ARMS, conditions=dict(chunk_cache='identical frozen copies'))
        if actual_fixture:cfg['source_revision']='fixture core05c9760; synthetic opaque bytes, not model performance'
        self.write(root/'config.json', dict(cfg,status='not_run'))
        cfg['config_source_sha256']=e.file_sha(root/'config.json')
        self.write(root/'frozen.json', cfg)
        (cohort_root/'frozen.json').write_bytes((root/'frozen.json').read_bytes())
        rows=[]
        for index,(block,arm) in enumerate((b,a) for b,order in enumerate(t.orders(n),1) for a in order):
            run_id='block-%02d-%s'%(block,arm)
            remote=PurePosixPath('/runs/trio88-stage-1e99504/actual-fixture')/label/run_id
            trial=cohort_root/run_id
            trial.mkdir()
            start=(index+1)*100_000_000_000
            argv=['nice','-n','19']+t.argv(cfg,arm,remote)
            timed=(t.h.trace_argv(remote/'deploy.trace') if traced else [])+argv
            resources=('Command being timed: "'+' '.join(timed)+'"\nUser time (seconds): 0.50\n'
                'System time (seconds): 0.10\nMaximum resident set size (kbytes): 100\n'
                'File system inputs: 0\nFile system outputs: 0\n'
                'Elapsed (wall clock) time (h:mm:ss or m:ss): 0:01.00\nExit status: 0\n')
            command=dict(argv=argv,executed_argv=['/usr/bin/time','-v','-o',str(remote/'deploy.resources')]+timed,
                gnu_time=resources,resources=t.h.time_metrics(resources),status='succeeded',exit_code=0,
                elapsed_s=1.,traced=traced,monotonic_start_ns=start,monotonic_end_ns=start+1_000_000_000,
                environment=dict(GOMAXPROCS='32',GOMEMLIMIT=None,LC_ALL='C',TZ='UTC',TMPDIR=str(remote/'tmp')))
            cli=dict(policy=arm,manifest=cfg['target_manifest'],claim_calls=0 if arm=='P' else 1,admitted=arm=='R',
                stats=dict(base=cfg['base_manifest'] if arm=='R' else None,reused=9 if arm=='R' else 0,
                local_base=cfg['base_manifest'] if arm=='C' else None,local_bytes=9 if arm=='C' else 0,
                read_bytes=0,peer_bytes=0,cached=0,duration_ns=500_000_000))
            def copies(inventory,offset):
                return [dict(x,independent_inode=True,nlink=1,source_dev=1,source_ino=offset,
                    copy_dev=1,copy_ino=offset+1) for x in inventory]
            row=dict(arm=arm,status='succeeded',valid=True,treatment_valid=True,output_hash_equal=True,
                output_inventory=cfg['target_inventory'],command=command,cli=cli,
                base_copy_proof=copies(cfg['base_inventory'],20),cache_copy_proof=copies(cfg['cache_inventory'],30),
                donor_proof=dict(base_identity_before=[1,21],base_inventory_after=[] if arm=='R' else cfg['base_inventory'],
                    base_identity_after=None if arm=='R' else [1,21],target_identity=[1,21] if arm=='R' else [1,40]),
                sync_start_ns=start+1_000_000_000,sync_end_ns=start+1_100_000_000,
                validation_start_ns=start+1_100_000_000,validation_end_ns=start+1_200_000_000,
                donor_validation_start_ns=start+1_200_000_000,donor_validation_end_ns=start+1_300_000_000,
                preparation_s=.5,preparation_inclusive_s=1.7,donor_validation_s=.1,durable_s=1.1,validated_s=1.2,
                physical_io=None,logical_io=None)
            if traced:
                lines=['pwrite64(4<'+str(remote/'output/target.rbln')+'>, "", '+('1' if arm=='R' else '10')+', 0) = '+('1' if arm=='R' else '10'),
                       'fsync(4<'+str(remote/'output/target.rbln')+'>) = 0']
                if arm=='C':lines+=['pread64(3<'+str(remote/'base/base.rbln')+'>, "", 9, 0) = 9']
                (trial/'deploy.trace.1').write_bytes(('\r\n'.join(lines)+'\r\n').encode())
                row['trace_accounting']=t.h.read_traces(trial/'deploy.trace',t.h.trace_roots(cfg,remote))
                row['logical_io']=copy.deepcopy(row['trace_accounting']['groups'])
            self.write(trial/'result.json',row)
            self.write(trial/'deploy.json',command)
            self.write(trial/'deploy.stdout',cli)
            (trial/'deploy.stderr').write_bytes(b'')
            (trial/'deploy.resources').write_bytes(resources.encode())
            self.write(trial/'cleanup.json',dict(policy='remove successful trial artifact trees; retain raw evidence'))
            rows.append(dict(row,run_id=run_id))
        cohort=dict(stage=stage,traced=traced,status='succeeded',valid=True,arms=t.ARMS,order=t.orders(n),trials=rows,
            frozen_sha256=hashlib.sha256((root/'frozen.json').read_bytes()).hexdigest(),physical_io=None,
            summary=t.h.summarize(rows,t.ARMS,n))
        self.write(cohort_root/'cohort.json',cohort)
        return label

    def test_exact_archive_roundtrip_excludes_payload_and_preserves_crlf(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);source=root/'source';source.mkdir();label=self.records(source,True)
            payload=source/label/'block-01-C/output/private-model';payload.parent.mkdir();payload.write_bytes(b'do not export model')
            manifest=e.pack(source,label,root/'package',part_size=4096)
            self.assertGreater(len(manifest['archive']['parts']),1)
            self.assertFalse(any('private-model' in r['path'] for r in manifest['files']))
            e.extract(root/'package',root/'extracted',label)
            path=Path(label)/'block-01-C/deploy.trace.1'
            self.assertEqual((source/path).read_bytes(),(root/'extracted'/path).read_bytes())
            result=e.verify_contents(root/'extracted',label,fixture=True)
            self.assertTrue(result['valid']);self.assertEqual(result['trials'],3)
            with self.assertRaises((AssertionError,ValueError,FileExistsError)):
                e.pack(source,label,root/'package')

    def test_verified_source_and_complete_untraced_witnesses(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);label=self.records(root)
            result=e.verify_contents(root,label,fixture=True)
            self.assertTrue(result['valid']);self.assertIsNone(result['logical_io'])
            self.assertEqual(result['n_per_arm'],1)

    def test_actual_frozen_fixture_uses_traced_smoke_without_io_prefix(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);label=self.records(root,True,actual_fixture=True)
            self.assertEqual(label,'smoke')
            result=e.verify_contents(root,label,fixture=True)
            self.assertTrue(result['valid']);self.assertFalse(result['model_evidence'])
            self.assertIsNotNone(result['logical_io'])

    def test_wrong_clock_branch_output_or_summary_cannot_pass(self):
        mutations=[lambda c:c['trials'][1]['cli']['stats'].update(local_bytes=0),
                   lambda c:c['trials'][1].update(validated_s=.1),
                   lambda c:c['trials'][1].update(output_inventory=[]),
                   lambda c:c['summary']['C'].update(valid=False),
                   lambda c:c.update(trials=c['trials'][:2])]
        for index,mutate in enumerate(mutations):
            with self.subTest(mutate=mutate),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);label=self.records(root);p=root/label/'cohort.json'
                cohort=json.loads(p.read_bytes());mutate(cohort)
                # Re-sign mirrored records and summaries: a failure must come from
                # the underlying constraint, not only a duplicate-record mismatch.
                for row in cohort['trials']:
                    trial=root/label/row['run_id']
                    self.write(trial/'result.json',{k:v for k,v in row.items() if k!='run_id'})
                    self.write(trial/'deploy.stdout',row['cli'])
                if index!=3:cohort['summary']=t.h.summarize(cohort['trials'],t.ARMS,1)
                self.write(p,cohort)
                with self.assertRaises((AssertionError,ValueError)):
                    e.verify_contents(root,label,fixture=True)

    def test_model_verification_refuses_synthetic_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);label=self.records(root)
            with self.assertRaises((AssertionError,ValueError)):
                e.verify_contents(root,label)

    def test_missing_donor_read_cannot_hide_behind_rewritten_counters(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);label=self.records(root,True)
            path=root/label/'block-01-C/deploy.trace.1'
            path.write_bytes(b'\r\n'.join(x for x in path.read_bytes().split(b'\r\n') if not x.startswith(b'pread64')))
            c=json.loads((root/label/'cohort.json').read_bytes());row=c['trials'][1]
            remote=PurePosixPath('/runs/trio88-stage-1e99504/actual-fixture')/label/'block-01-C'
            cfg=json.loads((root/'frozen.json').read_bytes())
            row['trace_accounting']=t.h.read_traces(path.parent/'deploy.trace',t.h.trace_roots(cfg,remote))
            row['logical_io']=copy.deepcopy(row['trace_accounting']['groups'])
            self.write(path.parent/'result.json',{k:v for k,v in row.items() if k!='run_id'})
            c['summary']=t.h.summarize(c['trials'],t.ARMS,1);self.write(root/label/'cohort.json',c)
            with self.assertRaises((AssertionError,ValueError)):
                e.verify_contents(root,label,fixture=True)

    def archive(self, root, name='smoke/record.json'):
        raw=b'{"synthetic":true}\r\n';buf=io.BytesIO()
        with tarfile.open(fileobj=buf,mode='w:gz') as tar:
            info=tarfile.TarInfo(name);info.size=len(raw);tar.addfile(info,io.BytesIO(raw))
        data=buf.getvalue();part=root/'evidence.tar.gz.part-000';part.write_bytes(data)
        m=dict(schema='onggi.three-arm-evidence.v1',cohort='smoke',
            files=[dict(path=name,bytes=len(raw),sha256=e.digest(raw))],
            archive=dict(bytes=len(data),sha256=e.digest(data),parts=[dict(file=part.name,bytes=len(data),sha256=e.digest(data))]))
        self.write(root/'export-manifest.json',m)
        return m

    def test_signed_archive_path_escape_and_corrupt_part_are_rejected(self):
        for name in ['../escaped','/absolute','smoke\\record.json']:
            with self.subTest(name=name),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);self.archive(root,name)
                with self.assertRaises(AssertionError):e.extract(root,root/'out','smoke')
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);self.archive(root)
            part=root/'evidence.tar.gz.part-000';b=part.read_bytes();part.write_bytes(b[:-1]+bytes([b[-1]^1]))
            with self.assertRaises(AssertionError):e.extract(root,root/'out','smoke')

    def test_omitted_or_duplicate_declared_record_is_rejected(self):
        for duplicate in [False,True]:
            with self.subTest(duplicate=duplicate),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);m=self.archive(root)
                m['files'].append(dict(m['files'][0],path=m['files'][0]['path'] if duplicate else 'smoke/absent.json'))
                self.write(root/'export-manifest.json',m)
                with self.assertRaises(AssertionError):e.extract(root,root/'out','smoke')

    def test_main_requires_exact_matching_smoke_and_all_fifteen_attempts(self):
        with self.assertRaisesRegex(AssertionError,'matching smoke'):
            e.verify(Path('absent'), 'main', fixture=True)
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);source=root/'source';source.mkdir()
            self.records(source);self.records(source,stage='main')
            e.pack(source,'smoke',root/'smoke-pack');e.pack(source,'main',root/'main-pack')
            result=e.verify(root/'main-pack','main',fixture=True,smoke_root=root/'smoke-pack')
            self.assertEqual(result['trials'],15);self.assertEqual(result['n_per_arm'],5)
            self.assertEqual(result['excluded_smoke']['trials'],3)
            # Keep every smoke witness correct but change its frozen configuration.
            cfg=json.loads((source/'frozen.json').read_bytes());cfg['different_start']='synthetic mutation'
            self.write(source/'frozen.json',cfg)
            (source/'smoke/frozen.json').write_bytes((source/'frozen.json').read_bytes())
            c=json.loads((source/'smoke/cohort.json').read_bytes())
            c['frozen_sha256']=e.file_sha(source/'frozen.json');self.write(source/'smoke/cohort.json',c)
            e.pack(source,'smoke',root/'changed-smoke-pack')
            with self.assertRaisesRegex(AssertionError,'starting conditions differ'):
                e.verify(root/'main-pack','main',fixture=True,smoke_root=root/'changed-smoke-pack')

    def test_preparation_has_no_retained_start_timestamp_and_is_not_certified_replay(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);label=self.records(root);c=json.loads((root/label/'cohort.json').read_bytes())
            row=c['trials'][1];row['preparation_s']=0.;row['preparation_inclusive_s']=86400.
            self.write(root/label/row['run_id']/'result.json',{k:v for k,v in row.items() if k!='run_id'})
            c['summary']=t.h.summarize(c['trials'],t.ARMS,1);self.write(root/label/'cohort.json',c)
            result=e.verify_contents(root,label,fixture=True)
            self.assertNotIn('preparation_s',result['timing']['C'])
            self.assertNotIn('preparation_inclusive_s',result['timing']['C'])
            self.assertEqual(result['worker_reported_timing']['C']['preparation_s']['values'],[0.])
            self.assertEqual(result['worker_reported_timing']['C']['preparation_inclusive_s']['values'],[86400.])
            self.assertIn('not independently reconstructed',result['preparation_scope'])

    def test_memory_limit_change_hidden_in_mirrored_command_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);label=self.records(root);c=json.loads((root/label/'cohort.json').read_bytes())
            row=c['trials'][1];row['command']['environment']['GOMEMLIMIT']='1GiB'
            trial=root/label/row['run_id'];self.write(trial/'result.json',{k:v for k,v in row.items() if k!='run_id'})
            self.write(trial/'deploy.json',row['command'])
            c['summary']=t.h.summarize(c['trials'],t.ARMS,1);self.write(root/label/'cohort.json',c)
            with self.assertRaises(AssertionError):e.verify_contents(root,label,fixture=True)

    def test_configuration_bytes_named_by_freeze_must_match(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);label=self.records(root)
            p=root/'config.json';p.write_bytes(p.read_bytes()+b' ')
            with self.assertRaises(AssertionError):e.verify_contents(root,label,fixture=True)


if __name__=='__main__':unittest.main()
