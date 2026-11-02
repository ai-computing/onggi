"""Offline byte/clock/output-witness checks; absent model bytes are not rehashed."""
import argparse
import base64
import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path, PurePosixPath
import subprocess
import tarfile


def digest(data):
    return hashlib.sha256(data).hexdigest()


def check_command(command, argv, resource_path, resources, h):
    assert command['status'] == 'succeeded' and command['exit_code'] == 0
    assert command['argv'] == ['nice', '-n', '19'] + argv
    assert command['executed_argv'] == ['/usr/bin/time', '-v', '-o', str(resource_path)] + command['argv']
    assert command['gnu_time'] == resources
    assert command['resources'] == h.time_metrics(resources)
    assert 'Command being timed: "' + ' '.join(command['argv']) + '"' in resources
    line = next(line for line in resources.splitlines() if line.strip().startswith('Elapsed (wall clock) time '))
    wall = 0.0
    for component in line.rsplit('):', 1)[1].strip().split(':'):
        wall = wall * 60 + float(component)
    # GNU time rounds to centiseconds and excludes outer launch/recording.
    # This loose consistency allowance never changes or subtracts measured time.
    assert wall - .02 <= command['elapsed_s'] <= wall + 1
    assert 'Exit status: 0' in resources


def verify_provenance(root):
    control = root / 'control'
    verify_controls(control)
    proof = control / 'proof'
    manifest = json.loads((proof / 'manifest.json').read_bytes())
    for row in manifest['files']:
        path = (proof / row['path']).resolve()
        assert path.is_relative_to(proof.resolve())
        raw = path.read_bytes()
        assert len(raw) == row['bytes'] and digest(raw) == row['sha256']
    cfg = json.loads((root / 'timing-main/timing-frozen.json').read_bytes())
    config_raw = (proof / 'cpu-model-a01/store/config/st/store').read_bytes()
    config_entry = next(x for x in cfg['store_inventory'] if x['path'] == 'config/st/store')
    assert digest(config_raw) == config_entry['sha256'] and len(config_raw) == config_entry['bytes']
    config = json.loads(config_raw)
    assert config['chunker'] == {'Min': 16384, 'Avg': 65536, 'Max': 262144}
    assert config['sub_chunker'] == {'Min': 1024, 'Avg': 4096, 'Max': 16384}
    assert config['frame'] == 262144 and config['pack_size'] == 67108864
    assert config['sub_kinds'] == ['atom-prefill', 'atom-decode', 'atom-other']
    staged = json.loads((control / 'cpu-model-v2-stage-manifest.json').read_bytes())
    assert digest((proof / 'stage/cpu_prepare_model_v2.py').read_bytes()) == staged['sha256']
    build = proof / 'evidence/build-02'
    assert (build / 'build-status.txt').read_text().strip() == 'completed'
    version = (build / 'go-version.txt').read_text().strip()
    assert version == 'go version ' + cfg['go_version']
    binaries = dict(reversed(line.split()) for line in (build / 'binary-sha256.txt').read_text().splitlines())
    assert binaries['/work/bin/onggi'] == cfg['binary_sha256']
    created = json.loads((control / 'cdx-resident-build-02-create.json').read_bytes())
    cleaned = json.loads((control / 'cdx-resident-build-02-cleanup.json').read_bytes())
    job = json.loads((control / 'cdx-resident-build-02-job.json').read_bytes())
    assert created['job_uid'] == cleaned['job_uid'] and cleaned['job_absent'] and cleaned['pods_absent']
    observed = cleaned['observations'][0]
    assert observed['phase'] == 'Succeeded' and observed['containers'][0]['state']['terminated']['exitCode'] == 0
    container = job['spec']['template']['spec']['containers'][0]
    assert container['workingDir'] == '/work/onggi-source'
    assert 'go build -mod=readonly -o /work/bin/onggi ./cmd/onggi' in container['command'][-1]
    assert observed['containers'][0]['imageID'].endswith('@' + container['image'].split('@')[1])
    archive = (proof / 'stage/onggi-source.tar.gz').read_bytes()
    assert digest(archive) == '94777fdd3847db4d8ca77ae7cb463731538c720a4f32ba445b9f3c1b90f231e8'
    core = '05c97605b07882f9ceaaeb90b94878f8923f81cf'
    assert cfg['source_revision'] == core
    expected = subprocess.check_output(['git', '-c', 'core.autocrlf=false', '-C', str(Path(__file__).resolve().parents[2]),
        'archive', '--format=tar', core, 'cmd', 'pkg', 'go.mod', 'go.sum'])
    def source_files(data):
        with tarfile.open(fileobj=io.BytesIO(data)) as tar:
            return {m.name: tar.extractfile(m).read().replace(b'\r\n', b'\n') for m in tar if m.isfile()}
    actual_files = source_files(archive)
    assert actual_files == source_files(expected), 'Source canonicalization applies only to code, never raw records'
    source = root / 'timing-main/timing-main/causal.py'
    spec = importlib.util.spec_from_file_location('provenance_frozen_cpu', source)
    assert digest(source.read_bytes()) == cfg['harness_sha256']
    h = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(h)
    remote_root = PurePosixPath(cfg['store']).parent
    for ref, role in [('a', 'base'), ('b', 'target')]:
        label = 'prepare-push-' + ref
        command = json.loads((proof / 'push' / (label + '.json')).read_bytes())
        check_command(command, h.publish_argv(cfg, 'TA', remote_root, ref, cfg[role + '_dir']),
                      remote_root / (label + '.resources'), (proof / 'push' / (label + '.resources')).read_text(), h)
        output = json.loads((proof / 'push' / (label + '.stdout')).read_bytes())
        assert output['manifest'] == cfg[role + '_manifest']
    return dict(verified=True, source_revision=core, canonical_source_files=len(actual_files),
                binary_sha256=cfg['binary_sha256'], go_version=cfg['go_version'], build_job_uid_cleaned=True)


def verify_controls(root):
    manifest = json.loads((root / 'manifest.json').read_bytes())
    for entry in manifest['files']:
        path = (root / entry['path']).resolve()
        assert path.is_relative_to(root.resolve())
        data = path.read_bytes()
        assert len(data) == entry['bytes'] and digest(data) == entry['sha256']
    names = ['cdx-resident-cpu-model-a01', 'cdx-resident-cpu-timing-smoke', 'cdx-resident-cpu-timing-main']
    for name in names:
        submitted = json.loads((root / (name + '-job.json')).read_bytes())
        created = json.loads((root / (name + '-create.json')).read_bytes())
        cleaned = json.loads((root / (name + '-cleanup.json')).read_bytes())
        actual = json.loads((root / (name + '-latest-status.json')).read_bytes())
        assert created['job_uid'] == cleaned['job_uid'] == actual['job']['metadata']['uid']
        assert cleaned['job_absent'] and cleaned['pods_absent']
        assert submitted['metadata']['name'] == actual['job']['metadata']['name'] == name
        for job in (submitted, actual['job']):
            spec = job['spec']
            assert spec['backoffLimit'] == 0 and spec['activeDeadlineSeconds'] > 0 and spec['ttlSecondsAfterFinished'] > 0
            assert spec['template']['spec']['restartPolicy'] == 'Never'
            for container in spec['template']['spec']['containers']:
                assert not container.get('securityContext', {}).get('privileged', False)
                resources = container['resources']
                assert resources['limits']['cpu'] == '12' and resources['limits']['memory'] == '32Gi'
                assert set(resources['limits']) == set(resources['requests']) == {'cpu', 'memory'}
                assert resources['requests'] == {'cpu': '4', 'memory': '8Gi'}
        observations = cleaned['observations']
        assert len(observations) == 1 and observations[0]['phase'] == 'Succeeded'
        assert [p['metadata']['uid'] for p in actual['pods']] == [observations[0]['uid']]
        assert all(c['state']['terminated']['exitCode'] == 0 for c in observations[0]['containers'])
    return dict(verified=True, cpu_jobs_uid_cleaned=len(names), control_files=len(manifest['files']))


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
    remote_root = PurePosixPath(cfg['store']).parent / label
    cfg['delta_file'] = str(remote_root / 'delta.vcdiff')
    if 'xdelta' in cohort['arms']:
        encode = ['xdelta3', '-e', '-s', str(PurePosixPath(cfg['base_dir']) / cfg['base_inventory'][0]['path']),
                  str(PurePosixPath(cfg['target_dir']) / cfg['target_inventory'][0]['path']), cfg['delta_file']]
        check_command(cohort['xdelta_publication'], encode, remote_root / 'encode-xdelta.resources',
                      (cohort_root / 'encode-xdelta.resources').read_text(), h)
    previous_end = None
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
            remote_trial = remote_root / run_id
            check_command(command, h.deploy_argv(cfg, arm, remote_trial), remote_trial / 'deploy.resources',
                          (trial / 'deploy.resources').read_text(), h)
            assert command['environment']['GOMAXPROCS'] == '32'
            assert command['elapsed_s'] == (command['monotonic_end_ns'] - command['monotonic_start_ns']) / 1e9
            stamps = [command['monotonic_start_ns'], command['monotonic_end_ns'],
                      row['sync_start_ns'], row['sync_end_ns'], row['validation_start_ns'], row['validation_end_ns']]
            assert stamps == sorted(stamps)
            assert previous_end is None or previous_end <= stamps[0]
            previous_end = stamps[-1]
            assert row['durable_s'] == (stamps[3] - stamps[0]) / 1e9
            assert row['validated_s'] == (stamps[5] - stamps[0]) / 1e9
            if arm in ('P', 'R'):
                cli = json.loads((trial / 'deploy.stdout').read_bytes())
                assert cli == row['cli'] and row['consume_treatment_valid']
                assert cli['stats'].get('base') == (cfg['base_manifest'] if arm == 'R' else None)
                assert 0 <= cli['stats']['duration_ns'] / 1e9 <= command['elapsed_s']
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
    parser.add_argument('--root', type=Path, default=Path(__file__).parent / 'data/20261102')
    args = parser.parse_args()
    result = verify(args.root / args.label, args.label)
    result['control'] = verify_controls(args.root / 'control')
    result['provenance'] = verify_provenance(args.root)
    print(json.dumps(result, indent=2))
