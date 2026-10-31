"""Verify the published E11 package and reproduce summaries from original raw data."""
import argparse
import json
import pathlib

from common import BodyCounter, aggregate, file_sha, save, validate_cohort, validate_ready


def verify(root):
    root=pathlib.Path(root).resolve()
    package=json.loads((root/'package-manifest.json').read_text())
    for item in package['files']:
        path=(root/item['path']).resolve()
        assert path.is_relative_to(root) and path.is_file()
        assert path.stat().st_size==item['bytes'] and file_sha(path)==item['sha256'],item['path']
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
    summary=aggregate(cohorts)
    assert len(summary['groups'])==4 and all(g['cohort_completion_s']['n']==5 for g in summary['groups'])
    summary['runtime_source']=inputs['source_commit']
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
