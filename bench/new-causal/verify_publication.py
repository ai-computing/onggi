"""Verify the exact two-file publication cohort; never rehash absent model payloads."""
import argparse
import base64
import gzip
import json
from pathlib import Path, PurePosixPath
import statistics
from types import ModuleType

from verify_timing import check_command, digest


ARMS = ['C', 'F', 'T', 'TF', 'TA']
FROZEN_SHA = '30e57a12c17e509aacef7749683f322f933a3533f487eceea521dbcbf0fe1ceb'
HARNESS_SHA = '1546801671fc19d3871487b2d2d4f0f8e445bae82cbfd25aabf9e87351554f5a'


def distribution(values):
    q = statistics.quantiles(values, method='inclusive') if len(values) > 1 else [values[0]] * 3
    return dict(values=values, n=len(values), median=statistics.median(values), q1=q[0], q3=q[2], iqr=q[2]-q[0])


def check_inventory(snapshot, kinds):
    aggregate = {kind: dict(bytes=0, objects=0) for kind in kinds | {'tmp'}}
    paths = set()
    for item in snapshot['objects']:
        p = PurePosixPath(item['path'])
        assert not p.is_absolute() and '..' not in p.parts and p.parts
        assert item['path'] not in paths and type(item['bytes']) is int and item['bytes'] >= 0
        paths.add(item['path'])
        assert p.parts[0] in aggregate
        aggregate[p.parts[0]]['bytes'] += item['bytes']
        aggregate[p.parts[0]]['objects'] += 1
    assert aggregate == snapshot['kinds'], 'Object inventory does not explain totals'
    finalized = sum(aggregate[k]['bytes'] for k in kinds)
    assert snapshot['pack_bytes'] == aggregate['packs']['bytes']
    assert snapshot['metadata_bytes'] == finalized - snapshot['pack_bytes']
    assert snapshot['finalized_bytes'] == finalized and snapshot['tmp_bytes'] == aggregate['tmp']['bytes']


def check_config(config, arm):
    coarse = dict(Min=16384, Avg=65536, Max=262144)
    fine = dict(Min=1024, Avg=4096, Max=16384)
    assert config['schema'] == 'onggi.store.v1' and config['pack_size'] == 64 << 20
    assert config['chunker'] == (fine if arm == 'F' else coarse)
    assert config.get('frame', 0) == (256 << 10 if arm in {'TF', 'TA'} else 0)
    if arm in {'T', 'TF', 'TA'}:
        assert config['sub_chunker'] == fine
        assert config['sub_kinds'] == ['atom-prefill', 'atom-decode', 'atom-other']
    else:
        assert not config.get('sub_chunker') and not config.get('sub_kinds')


def verify(root, label, smoke=None):
    root = Path(root).resolve()
    assert label in ('publish-smoke', 'publish-main')
    manifest = json.loads((root/'export-manifest.json').read_bytes())
    packed = (root/'exact-evidence.json.gz').read_bytes()
    assert len(packed) == manifest['archive']['bytes'] and digest(packed) == manifest['archive']['sha256']
    doc = json.loads(gzip.decompress(packed))
    assert doc['cohort'] == label, 'Label/stage mismatch'
    assert [{k: v for k, v in row.items() if k != 'base64'} for row in doc['files']] == manifest['files']
    assert len({row['path'] for row in doc['files']}) == len(doc['files']) == manifest['archive']['files']
    verified_records = {}
    for entry in doc['files']:
        relative = PurePosixPath(entry['path'])
        assert not relative.is_absolute() and '..' not in relative.parts
        path = (root/entry['path']).resolve()
        assert path.is_relative_to(root) and not (root/entry['path']).is_symlink()
        raw = path.read_bytes()
        assert len(raw) == entry['bytes'] and digest(raw) == entry['sha256']
        assert raw == base64.b64decode(entry['base64'], validate=True)
        verified_records[entry['path']] = raw

    def record(path):
        relative = path.relative_to(root).as_posix()
        assert relative in verified_records, 'Consumed record absent from exact archive: '+relative
        return verified_records[relative]

    cohort_root = root/label
    frozen = record(cohort_root/'frozen.json')
    assert digest(frozen) == FROZEN_SHA
    assert frozen == record(root/'publish-frozen.json')
    source = cohort_root/'causal.py'
    assert digest(record(source)) == HARNESS_SHA
    h = ModuleType('frozen_publication')
    h.__file__ = str(source)
    exec(compile(record(source), str(source), 'exec'), h.__dict__)
    cfg = json.loads(frozen)
    assert cfg['harness_sha256'] == HARNESS_SHA
    assert cfg['source_revision'] == '05c97605b07882f9ceaaeb90b94878f8923f81cf'
    assert cfg['binary_sha256'] == 'e6b8ee52a69c7cf9fe970cc1df08e93eab9c175c061baccb078725e549a15ae5'
    assert cfg['workers'] == 8 and cfg['gomaxprocs'] == 32 and cfg['publication_arms'] == ARMS
    cohort = json.loads(record(cohort_root/'cohort.json'))
    n = 5 if label == 'publish-main' else 1
    assert cohort['stage'] == label.split('-')[1] and cohort['experiment'] == 'publish'
    assert cohort['status'] == 'succeeded' and cohort['valid'] and not cohort['traced']
    assert cohort['frozen_sha256'] == FROZEN_SHA and cohort['arms'] == ARMS
    assert cohort['order'] == h.orders(ARMS, n)
    assert cohort['host']['/sys/fs/cgroup/cpu.max'] == '1200000 100000\n'
    assert cohort['host']['/sys/fs/cgroup/memory.max'] == '34359738368\n'
    if n == 5:
        assert smoke is not None, 'Main requires its separate verified smoke'
        verify(smoke, 'publish-smoke')
    expected = [(b, a) for b, order in enumerate(cohort['order'], 1) for a in order]
    assert len(cohort['trials']) == len(expected), 'Incomplete cohort'
    remote_root = PurePosixPath(cfg['base_dir']).parent/label
    previous_end = None
    for row, (block, arm) in zip(cohort['trials'], expected):
        run_id = 'block-%02d-%s' % (block, arm)
        assert row['run_id'] == run_id and row['arm'] == arm and row['valid'] and row['status'] == 'succeeded'
        trial = cohort_root/run_id
        assert {k: v for k, v in row.items() if k != 'run_id'} == json.loads(record(trial/'result.json'))
        check_config(row['stored_config'], arm)
        for key in ('after_a', 'after_b'):
            check_inventory(row[key], h.KINDS)
            assert row[key]['tmp_bytes'] == 0
        before = {item['path']: item['bytes'] for item in row['after_a']['objects']}
        after = {item['path']: item['bytes'] for item in row['after_b']['objects']}
        assert before.items() <= after.items(), 'Immutable A objects must survive B publication'
        assert {'config/st/store', 'refs/a'} <= before.keys()
        assert {'config/st/store', 'refs/a', 'refs/b'} <= after.keys()
        for snapshot in (row['after_a'], row['after_b']):
            assert all(snapshot['kinds'][kind]['objects'] > 0 for kind in h.KINDS)
        assert row['b_object_delta'] == {key: row['after_b'][key]-row['after_a'][key]
                                       for key in ('pack_bytes', 'metadata_bytes', 'finalized_bytes')}
        remote_trial = remote_root/run_id

        def command(name, argv, recorded=None):
            nonlocal previous_end
            value = json.loads(record(trial/(name+'.json')))
            if recorded is not None:
                assert value == recorded
            assert not value['traced']
            assert value['environment'] == dict(GOMAXPROCS='32', GOMEMLIMIT=None, LC_ALL='C',
                                                TMPDIR=str(remote_trial/'tmp'), TZ='UTC')
            start, end = value['monotonic_start_ns'], value['monotonic_end_ns']
            assert end >= start and value['elapsed_s'] == (end-start)/1e9
            assert previous_end is None or start >= previous_end
            previous_end = end
            check_command(value, argv, remote_trial/(name+'.resources'),
                          record(trial/(name+'.resources')).decode(), h)
            return value

        published = {}
        pack_bytes, packs = 0, 0
        for ref, role in [('a', 'base'), ('b', 'target')]:
            name = 'push-'+ref
            command(name, h.publish_argv(cfg, arm, remote_trial, ref, cfg[role+'_dir']), row['push_'+ref])
            output = json.loads(record(trial/(name+'.stdout')))
            assert output['ref'] == ref
            assert output['stats']['bytes'] == sum(x['bytes'] for x in cfg[role+'_inventory'])
            assert output['manifest'].startswith('sha256:')
            published[ref] = output['manifest']
            pack_bytes += output['stats']['pack_bytes']
            packs += output['stats']['packs']
            assert row['after_'+ref]['pack_bytes'] == pack_bytes, 'Pack bytes contradict push stdout'
            assert row['after_'+ref]['kinds']['packs']['objects'] == packs
            for kind, flags in [('apparent', ['-sb']), ('allocated', ['-sB1'])]:
                name = 'du-'+ref+'-'+kind
                command(name, ['du']+flags+[str(remote_trial/'store')], row['du_'+ref+'_'+kind])
                number, path = record(trial/(name+'.stdout')).decode().strip().split(maxsplit=1)
                assert int(number) >= 0
                if kind == 'apparent':
                    assert int(number) >= row['after_'+ref]['finalized_bytes']
                assert path == str(remote_trial/'store')
        assert row['b_publication_s'] == row['push_b']['elapsed_s']
        assert row['family_publication_s'] == row['push_a']['elapsed_s']+row['push_b']['elapsed_s']
        command('diff', [cfg['onggi'], 'diff', '--store', str(remote_trial/'store'), '--json', '--ranges', 'a', 'b'], row['diff'])
        delta = json.loads(record(trial/'diff.stdout'))
        target_bytes = sum(x['bytes'] for x in cfg['target_inventory'])
        assert delta['size'] == target_bytes and delta['same']+delta['moved']+delta['new'] == target_bytes
        for ref, role in [('a', 'base'), ('b', 'target')]:
            command('verify-'+ref, [cfg['onggi'], 'pull', '--store', str(remote_trial/'store'), '--workers', '8',
                                    '--json', ref, str(remote_trial/('verify-'+ref))])
            output = json.loads(record(trial/('verify-'+ref+'.stdout')))
            assert output['manifest'] == published[ref]
            assert output['stats']['bytes'] == sum(x['bytes'] for x in cfg[role+'_inventory'])
        cleanup = json.loads(record(trial/'cleanup.json'))
        assert cleanup['policy'] == 'remove successful trial artifact trees; retain raw evidence'
        assert set(cleanup['removed']) == {str(remote_trial/name) for name in ('store','tmp','verify-a','verify-b')}
    assert cohort['summary'] == h.summarize(cohort['trials'], ARMS, n, cohort_valid=True)
    metrics = {}
    for arm in ARMS:
        rows = [row for row in cohort['trials'] if row['arm'] == arm]
        metrics[arm] = {key: distribution([row['after_b'][key] for row in rows])
                        for key in ('pack_bytes','metadata_bytes','finalized_bytes')}
        metrics[arm].update({key: distribution([row[key] for row in rows])
                            for key in ('b_publication_s','family_publication_s')})
    return dict(schema='onggi.cpu-publication-acceptance.v1', verified=True, label=label,
                n_per_arm=n, trials=len(expected), archive_sha256=digest(packed),
                frozen_sha256=FROZEN_SHA, harness_sha256=HARNESS_SHA, summary=metrics,
                source_revision=cfg['source_revision'], binary_sha256=cfg['binary_sha256'],
                inputs={key:cfg[key+'_inventory'] for key in ('base','target')},
                environment=dict(workers=8, gomaxprocs=32, cpu_limit='12', memory_limit='32Gi',
                                 cache='warm/uncontrolled OS', locale='C', timezone='UTC'),
                independent_payload_rehash=False,
                correctness_scope='Pinned worker performed full inventory roundtrip before success; absent model/output bytes cannot be independently rehashed offline',
                comparison_scope='Two ATOM files only; F/T match zstd and frame0; T/TF change frames; TF/TA change codec selection; no flat-fine-with-frames arm or full-set isolation')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root', type=Path)
    parser.add_argument('label', choices=['publish-smoke','publish-main'])
    parser.add_argument('--smoke', type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    report = verify(args.root, args.label, args.smoke)
    assert not args.output.exists(), 'Preserve prior acceptance records'
    args.output.write_text(json.dumps(report, indent=2)+'\n', encoding='utf-8')
    print(json.dumps(dict(verified=True, trials=report['trials'], n_per_arm=report['n_per_arm'])))
