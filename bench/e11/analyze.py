"""Verify the published E11 package and reproduce summaries from original raw data."""
import argparse
import hashlib
import json
import pathlib

from common import BodyCounter, aggregate, distribution, file_sha, save, validate_cohort, validate_ready


def verify(root):
    root=pathlib.Path(root).resolve()
    package=json.loads((root/'package-manifest.json').read_text())
    assert {p.relative_to(root).as_posix() for p in root.rglob('*') if p.is_file() and p.name!='package-manifest.json'}=={i['path'] for i in package['files']}
    for item in package['files']:
        path=(root/item['path']).resolve()
        assert path.is_relative_to(root) and path.is_file()
        assert path.stat().st_size==item['bytes'] and file_sha(path)==item['sha256'],item['path']
    identity=json.loads((root/'source-identity.json').read_text())
    assert identity['executed_harness_commit']=='7870cef872e201a6a4dca87ed1bb998da9c3109f'
    configmap=json.loads((root/'dell/control/cdx-e11-scripts-03-created.json').read_text())['data']
    for name,item in identity['source_files'].items():
        for path in [pathlib.Path(__file__).parent/name,root/'dell'/name]:
            raw=path.read_bytes().replace(b'\r\n',b'\n')
            assert len(raw)==item['bytes'] and hashlib.sha256(raw).hexdigest()==item['canonical_lf_sha256'],str(path)
        if name in configmap:
            assert hashlib.sha256(configmap[name].encode()).hexdigest()==item['canonical_lf_sha256']
    origin=root/'etri/attempt03'
    inputs=json.loads((origin/'inputs.json').read_text())
    workers=json.loads((origin/'workers.json').read_text())
    expected={'base':'sha256:04b45065cea8408e08043c1cbd13bdc4aa230c9683a9d85ed97f5fb1e5567526',
              'target':'sha256:66cedc7b2a1389394c7ca5275d171625993c23742d7c7834bcb7e93a2be52e52'}
    assert {k:v['manifest'] for k,v in inputs['artifacts'].items()}==expected
    assert set(workers)=={'cpu-1','cpu-2','etri-001'}
    for worker in workers.values():
        assert not worker['device_files'] and worker['bootstrap']['hashes_match']
        assert worker['binary_sha256']==inputs['tools']['onggi']['sha256']
    seed=workers['cpu-1']['seed']
    assert seed['hashes_match'] and seed['returncode']==0
    counters={}
    requests=[]
    for line in (origin/'origin-requests.jsonl').read_text().splitlines():
        row=json.loads(line)
        requests.append(row)
        counter=counters.setdefault(row['trial'],BodyCounter())
        counter.record(row['path'],row['body_bytes'],row['status'],bool(row['transfer_error']))
    cohorts=[]
    for path in sorted((origin/'runs').glob('*/cohort.json')):
        c=json.loads(path.read_text())
        validate_ready(c,inputs,workers['cpu-1']['seed_cache_inventory'])
        assert c['base_manifest']==expected['base'] and c['target_manifest']==expected['target']
        assert c['origin']==counters[c['trial']].snapshot(),c['trial']
        assert c['metrics']==validate_cohort(c)
        controller=json.loads((root/'dell/cohorts'/(c['trial']+'.json')).read_text())
        assert controller==c,'controller/origin raw copies differ'
        for node,result in c['results'].items():
            assert result['files']==sorted(inputs['artifacts']['target']['files'],key=lambda x:x['path'])
            assert result['trial_payloads_absent']
            assert result['pod_uid']==workers[node]['pod_uid']
        cohorts.append(c)
    assert len(cohorts)==24 and sum(c['stage']=='smoke' for c in cohorts)==4
    terminal=json.loads((root/'dell/final-terminal-api.json').read_text())
    assert len(terminal['jobs'])==4 and len(terminal['pods'])==4
    job_uids={j['metadata']['uid'] for j in terminal['jobs']}
    pod_uids={p['metadata']['uid'] for p in terminal['pods']}
    assert {w['pod_uid'] for w in workers.values()} < pod_uids
    by_uid={p['metadata']['uid']:p for p in terminal['pods']}
    assert {by_uid[w['pod_uid']]['spec']['nodeName'] for w in workers.values()}==set(workers)
    for node,worker in workers.items():
        assert by_uid[worker['pod_uid']]['spec']['nodeName']==node
    for job in terminal['jobs']:
        assert job['metadata']['name'].startswith('cdx-e11-')
        assert job['spec']['backoffLimit']==0 and job['spec']['activeDeadlineSeconds']==14400
        assert job['spec']['ttlSecondsAfterFinished']==600
        source=job['metadata']['annotations']['e11.source-sha']
        assert len(source)>=7 and identity['executed_harness_commit'].startswith(source)
    for pod in terminal['pods']:
        assert pod['status']['phase']=='Succeeded'
        assert any(o['uid'] in job_uids for o in pod['metadata']['ownerReferences'])
        assert not pod['spec'].get('hostNetwork',False)
        for c in pod['spec']['containers']:
            assert set(c['resources']['limits']) <= {'cpu','memory','ephemeral-storage'}
            assert set(c['resources']['requests']) <= {'cpu','memory','ephemeral-storage'}
            assert not c.get('securityContext',{}).get('privileged',False)
            assert all(m['readOnly'] for m in c['volumeMounts'] if m['name']=='store')
        for v in pod['spec']['volumes']:
            assert not v.get('hostPath',{}).get('path','').startswith('/dev')
            if v['name']=='scripts':
                assert v['configMap']['name']=='cdx-e11-scripts-03'
    cleanup=json.loads((root/'dell/final-cleanup.json').read_text())
    assert cleanup['jobs_absent'] and cleanup['pods_absent']
    assert set(cleanup['job_uids'])==job_uids and set(cleanup['pod_uids'])==pod_uids
    final=json.loads((root/'dell/final-owned-inventory.json').read_text())
    assert final['namespace_absent'] and not final['cdx_jobs'] and not final['cdx_pods']
    summary=aggregate(cohorts)
    assert len(summary['groups'])==4 and all(g['cohort_completion_s']['n']==5 for g in summary['groups'])
    for group in summary['groups']:
        selected=[c for c in cohorts if c['trial'] in group['trials']]
        group['per_node_completion_s']={n:distribution([c['pulled'][n]-c['release_mono'] for c in selected])
                                        for n in group['nodes']}
        group['per_node_base_copy_s']={n:distribution([c['ready'][n]['copy_s'] for c in selected])
                                       for n in group['nodes']}
        for field in ['hash_validation_s','cleanup_s']:
            group['per_node_'+field]={n:distribution([c['results'][n][field] for c in selected])
                                      for n in group['nodes']}
    summary['runtime_source']=inputs['source_commit']
    summary['statistical_unit']='one simultaneously released three-node cohort; n=5 per condition'
    summary['node_count']=3
    summary['main_cohorts']=20
    summary['node_samples']=60
    summary['owned_uid_cleanup_verified']=True
    summary['tools']=inputs['tools']
    summary['seed']={k:v for k,v in seed.items() if k not in ['files','stdout','stderr']}
    summary['seed']['report']=json.loads(seed['stdout'])
    summary['bootstrap_origin']=counters['bootstrap'].snapshot()
    summary['node_environment']={n:dict(nics=w['nics'],bootstrap_s=w['bootstrap']['wall_s'],
                                       hash_validation_s=w['bootstrap']['hash_validation_s'],python=w['python'])
                                 for n,w in workers.items()}
    summary['limitations']=[
        'N=3; one recipient colocated with origin; observed lab topology only.',
        'Preexisting peer snapshot, not dynamic peer dissemination during a pull.',
        'Independent trial base copies and full-file hashing outside timed pull.',
        'OS page cache and unrelated host/network load uncontrolled.',
        'Application response body/range/chunk payload counts, not physical NIC or disk bytes.',
        'Central completion includes marker HTTP receipt; post-pull hashes validated separately.',
        'Payloads absent from Git: offline verification compares worker-recorded full-file hashes with frozen inventories.',
        'No NPU/GPU allocation, inference, CSI mount-latency or cross-vendor comparison.']
    return summary


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=pathlib.Path,required=True)
    parser.add_argument('--out',type=pathlib.Path)
    args=parser.parse_args()
    summary=verify(args.root)
    if args.out:
        save(args.out,summary)
    print(json.dumps(dict(verified=True,groups=[dict(stratum=g['stratum'],arm=g['arm'],
        cohort_completion_s=g['cohort_completion_s'],store_read_bytes=g['store_read_bytes'],
        peer_bytes=g['peer_bytes']) for g in summary['groups']]),indent=2))
