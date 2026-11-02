"""Offline byte/clock/output-witness checks; absent model bytes are not rehashed."""
import argparse
import base64
import gzip
import hashlib
import importlib.util
import json
from pathlib import Path


def digest(data):
    return hashlib.sha256(data).hexdigest()


def verify(root, label):
    manifest = json.loads((root / 'export-manifest.json').read_bytes())
    packed = (root / 'exact-evidence.json.gz').read_bytes()
    assert digest(packed) == manifest['archive']['sha256'] and len(packed) == manifest['archive']['bytes']
    doc = json.loads(gzip.decompress(packed))
    assert doc['cohort'] == label
    assert [{k: v for k, v in row.items() if k != 'base64'} for row in doc['files']] == manifest['files']
    for entry in doc['files']:
        path = (root / entry['path']).resolve()
        assert path.is_relative_to(root.resolve())
        data = path.read_bytes()
        assert len(data) == entry['bytes'] and digest(data) == entry['sha256']
        assert data == base64.b64decode(entry['base64'])
    cohort_root = root / label
    copied_source = cohort_root / 'causal.py'
    assert digest(copied_source.read_bytes()) == '1546801671fc19d3871487b2d2d4f0f8e445bae82cbfd25aabf9e87351554f5a'
    spec = importlib.util.spec_from_file_location('frozen_cpu_harness', copied_source)
    h = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(h)
    cohort = json.loads((cohort_root / 'cohort.json').read_bytes())
    cfg = json.loads((cohort_root / 'frozen.json').read_bytes())
    assert cohort['experiment'] == 'deploy' and not cohort['traced'], 'This checker covers untraced timing only'
    assert cfg.get('cache_dir') is None and cfg['workers'] == 8 and cfg['gomaxprocs'] == 32
    assert cfg['base_inventory'] == [dict(path='decode_S256_B2_lm.rbln', bytes=15238278850,
        sha256='8919e9fea5c9fddc6f5941dbd80364790eeff1cc55b13c153c1a14991ee59332')]
    assert cfg['target_inventory'] == [dict(path='decode_S256_B8_lm.rbln', bytes=15228048882,
        sha256='304ff369a72c21afbbc84b7b09639503cd0e6327320a3ec23664d75e6c13460d')]
    assert cfg['harness_sha256'] == digest(copied_source.read_bytes())
    assert cfg['binary_sha256'] == 'e6b8ee52a69c7cf9fe970cc1df08e93eab9c175c061baccb078725e549a15ae5'
    assert digest((cohort_root / 'frozen.json').read_bytes()) == cohort['frozen_sha256']
    assert cohort['status'] == 'succeeded' and cohort['valid']
    n = 5 if cohort['stage'] == 'main' else 1
    assert cohort['order'] == h.orders(cohort['arms'], n)
    expected = [(block, arm) for block, arms in enumerate(cohort['order'], 1) for arm in arms]
    assert len(cohort['trials']) == len(expected)
    for row, (block, arm) in zip(cohort['trials'], expected):
        run_id = 'block-%02d-%s' % (block, arm)
        assert row['run_id'] == run_id and row['arm'] == arm
        trial = cohort_root / run_id
        assert row['status'] == 'succeeded' and row['valid']
        assert {k: v for k, v in row.items() if k != 'run_id'} == json.loads((trial / 'result.json').read_bytes())
        assert json.loads((trial / 'cleanup.json').read_bytes())['policy'] != 'retain'
        if cohort['experiment'] == 'deploy':
            assert row['output_hash_equal'] and row['output_inventory'] == cfg['target_inventory']
            assert row['base_copy_proof'] and all(p['independent_inode'] and p['nlink'] == 1
                and (p['source_dev'], p['source_ino']) != (p['copy_dev'], p['copy_ino']) for p in row['base_copy_proof'])
            command = row['command']
            assert command == json.loads((trial / 'deploy.json').read_bytes())
            assert command['status'] == 'succeeded' and command['exit_code'] == 0
            assert command['traced'] == cohort['traced']
            assert command['argv'][:3] == ['nice', '-n', '19']
            assert command['environment']['GOMAXPROCS'] == '32'
            assert command['elapsed_s'] == (command['monotonic_end_ns'] - command['monotonic_start_ns']) / 1e9
            stamps = [command['monotonic_start_ns'], command['monotonic_end_ns'],
                      row['sync_start_ns'], row['sync_end_ns'], row['validation_start_ns'], row['validation_end_ns']]
            assert stamps == sorted(stamps)
            assert row['durable_s'] == (stamps[3] - stamps[0]) / 1e9
            assert row['validated_s'] == (stamps[5] - stamps[0]) / 1e9
            if arm in ('P', 'R'):
                cli = json.loads((trial / 'deploy.stdout').read_bytes())
                assert cli == row['cli'] and row['consume_treatment_valid']
                assert cli['stats'].get('base') == (cfg['base_manifest'] if arm == 'R' else None)
            elif arm.startswith('rsync'):
                assert row['rsync_protocol'] == h.rsync_stats((trial / 'deploy.stdout').read_text())
                assert ('--inplace' in command['argv']) == (arm == 'rsync_inplace')
            if not cohort['traced']:
                assert row['logical_io'] is None
    summary = h.summarize(cohort['trials'], cohort['arms'], n)
    assert summary == cohort['summary'] and all(s['valid'] for s in summary.values())
    return dict(verified=True, label=label, n=n, trials=len(expected), files=len(doc['files']),
        status=cohort['status'], traced=cohort['traced'], summary={arm:{k:v for k,v in s.items() if k != 'samples'}
        for arm,s in summary.items()}, scope='Exact export bytes and recorded output/hash/inode/clock witnesses; model files absent, no independent full-byte rehash')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('label')
    args = parser.parse_args()
    result = verify(Path(__file__).parent / 'cpu-exports' / args.label, args.label)
    print(json.dumps(result, indent=2))
