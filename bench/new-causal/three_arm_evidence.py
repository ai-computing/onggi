"""Exact offline replay for the separately frozen P/C/R cohort.

Pack only after measurement stops. No model/store/cache payload is exported.
Verification replays archived sources and traces; absent model bytes are not
independently rehashed. Synthetic fixture mode never yields a model result.
"""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import statistics
import tarfile
import tempfile
from types import ModuleType

LABELS = ['smoke', 'main', 'io-smoke', 'io-main']
RUNNER_SHA = 'b04a44fe2928247e8b4c588e5b9a20e3fcd48c2f04b0bc9577e83a31bfe4abd9'
HELPER_SHA = '1546801671fc19d3871487b2d2d4f0f8e445bae82cbfd25aabf9e87351554f5a'
TIMING_SHA = 'cc4d9b41ff92787d28c1790a71cdee1ed786765fdc5e64a9c53e2bb2e40139e8'
DRIVER_SHA = '4613b8936144f4a13e22e9e47a4df700a73062e44615a6438741bd3d14634a53'
CALLER_SHA = 'b6015c42ea40772f57d0ac89e013597ad63201ac5ba0e61f7e2bb1183e7dab81'


def digest(data):
    return hashlib.sha256(data).hexdigest()


def file_sha(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


def checked_module(path, sha, name):
    path = Path(path)
    raw = path.read_bytes()
    assert digest(raw) == sha, 'Source bytes differ: '+str(path)
    module = ModuleType(name)
    module.__file__ = str(path)
    exec(compile(raw, str(path), 'exec'), module.__dict__)
    return module


def pack(source, label, output, part_size=48_000_000):
    source, output = Path(source).resolve(), Path(output).resolve()
    assert label in LABELS and type(part_size) is int and 0 < part_size <= 48_000_000
    assert not output.exists() and not output.is_relative_to(source) and not source.is_relative_to(output)
    cohort = source/label
    assert json.loads((cohort/'cohort.json').read_bytes())['status'] in ['succeeded', 'failed']
    suffixes = {'.json', '.py', '.stdout', '.stderr', '.resources'}
    paths = [p for p in cohort.iterdir() if p.is_file() and p.suffix in suffixes]
    for trial in cohort.glob('block-*'):
        if trial.is_dir():
            paths += [p for p in trial.iterdir() if p.is_file() and (p.suffix in suffixes or '.trace.' in p.name)]
    paths += [source/name for name in ['config.json', 'frozen.json', 'worker-source.py'] if (source/name).exists()]
    paths = sorted(set(paths))
    assert all(not p.is_symlink() and p.resolve().is_relative_to(source) for p in paths)
    output.mkdir(parents=True)
    rows = []
    archive_path = output/'evidence.tar.gz'
    class Reader:
        def __init__(self, stream):
            self.stream, self.count, self.sha = stream, 0, hashlib.sha256()
        def read(self, size):
            data = self.stream.read(size)
            self.count += len(data)
            self.sha.update(data)
            return data
    with tarfile.open(archive_path, 'w:gz') as archive:
        for path in paths:
            before = path.stat()
            info = tarfile.TarInfo(path.relative_to(source).as_posix())
            info.size, info.mode = before.st_size, 0o644
            with path.open('rb') as stream:
                reader = Reader(stream)
                archive.addfile(info, reader)
            after = path.stat()
            assert (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) == (
                after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns), 'Evidence changed during export'
            assert reader.count == before.st_size
            rows.append(dict(path=info.name, bytes=reader.count, sha256=reader.sha.hexdigest()))
    parts = []
    with archive_path.open('rb') as stream:
        for index, data in enumerate(iter(lambda: stream.read(part_size), b'')):
            name = 'evidence.tar.gz.part-%03d' % index
            (output/name).write_bytes(data)
            parts.append(dict(file=name, bytes=len(data), sha256=digest(data)))
    manifest = dict(schema='onggi.three-arm-evidence.v1', cohort=label, files=rows,
        archive=dict(bytes=archive_path.stat().st_size, sha256=file_sha(archive_path), parts=parts),
        scope='Exact raw/source bytes; large input/output/store/cache payload trees excluded')
    (output/'export-manifest.json').write_text(json.dumps(manifest, indent=2)+'\n', encoding='utf-8')
    return manifest


def extract(root, output, label):
    root, output = Path(root).resolve(), Path(output).resolve()
    assert label in LABELS and not output.exists()
    manifest = json.loads((root/'export-manifest.json').read_bytes())
    assert manifest['schema'] == 'onggi.three-arm-evidence.v1' and manifest['cohort'] == label
    expected = {r['path']: r for r in manifest['files']}
    assert expected and len(expected) == len(manifest['files'])
    output.mkdir(parents=True)
    # Reassemble outside the extracted tree; never permit an unarchived file to satisfy a witness.
    with tempfile.TemporaryFile() as packed:
        count, sha = 0, hashlib.sha256()
        for index, part in enumerate(manifest['archive']['parts']):
            assert part['file'] == 'evidence.tar.gz.part-%03d' % index
            path = root/part['file']
            assert not path.is_symlink() and 0 < part['bytes'] <= 48_000_000
            assert path.stat().st_size == part['bytes'] and file_sha(path) == part['sha256']
            with path.open('rb') as stream:
                for block in iter(lambda: stream.read(1 << 20), b''):
                    count += len(block); sha.update(block); packed.write(block)
        assert count == manifest['archive']['bytes'] and sha.hexdigest() == manifest['archive']['sha256']
        packed.seek(0)
        seen = set()
        with tarfile.open(fileobj=packed, mode='r|gz') as archive:
            for entry in archive:
                assert entry.isfile() and entry.name in expected and entry.name not in seen
                path = (output/entry.name).resolve()
                assert '\\' not in entry.name and not PurePosixPath(entry.name).is_absolute()
                assert path.is_relative_to(output), 'Archive path escape'
                row = expected[entry.name]
                assert entry.size == row['bytes']
                path.parent.mkdir(parents=True, exist_ok=True)
                count, sha = 0, hashlib.sha256()
                with archive.extractfile(entry) as src, path.open('xb') as dest:
                    for block in iter(lambda: src.read(1 << 20), b''):
                        count += len(block); sha.update(block); dest.write(block)
                assert count == row['bytes'] and sha.hexdigest() == row['sha256']
                seen.add(entry.name)
        assert seen == set(expected), 'Archive omitted declared evidence'
    return manifest


def check_copies(proof, inventory):
    assert [{k: row[k] for k in ['path', 'bytes', 'sha256']} for row in proof] == inventory
    for row in proof:
        assert row['independent_inode'] is True and row['nlink'] == 1
        assert (row['source_dev'], row['source_ino']) != (row['copy_dev'], row['copy_ino'])


def distribution(values):
    q1, _, q3 = statistics.quantiles(values, method='inclusive') if len(values) > 1 else [values[0]]*3
    return dict(values=values, median=statistics.median(values), q1=q1, q3=q3)


def verify_contents(root, label, fixture=False):
    """Only call on exact extracted data, or explicitly synthetic unit-test records."""
    root = Path(root)
    assert label in LABELS
    cohort_root = root/label
    assert digest((cohort_root/'causal.py').read_bytes()) == HELPER_SHA
    t = checked_module(cohort_root/'three_arm.py', RUNNER_SHA, 'archived_three_arm')
    v = checked_module(Path(__file__).with_name('verify_timing.py'), TIMING_SHA, 'reviewed_command_checks')
    cfg = json.loads((cohort_root/'frozen.json').read_bytes())
    assert (cohort_root/'frozen.json').read_bytes() == (root/'frozen.json').read_bytes()
    assert cfg['config_source_sha256'] == file_sha(root/'config.json'), 'Configuration bytes differ from freeze'
    frozen_sha = file_sha(root/'frozen.json')
    cohort = json.loads((cohort_root/'cohort.json').read_bytes())
    actual_fixture = fixture and cfg.get('source_revision') == 'fixture core05c9760; synthetic opaque bytes, not model performance'
    if actual_fixture:
        assert label == 'smoke', 'The frozen actual fixture has one traced smoke directory'
    traced = actual_fixture or label.startswith('io-')
    stage = 'smoke' if label.endswith('smoke') else 'main'
    n = 1 if stage == 'smoke' else 5
    assert cfg['workers'] == 8 and cfg['gomaxprocs'] == 32
    assert cfg['driver_sha256'] == DRIVER_SHA and cfg['driver_source_sha256'] == CALLER_SHA
    assert cfg['three_arm_sha256'] == RUNNER_SHA and cfg['harness_sha256'] == HELPER_SHA
    assert cfg['source_revision'] in (['05c97605b07882f9ceaaeb90b94878f8923f81cf',
        'fixture core05c9760; synthetic opaque bytes, not model performance'] if fixture
        else ['05c97605b07882f9ceaaeb90b94878f8923f81cf'])
    assert cfg['deployment_arms'] == t.ARMS and cfg['cache_inventory']
    assert cfg['conditions']['chunk_cache'] == 'identical frozen copies'
    if not fixture:
        assert frozen_sha == '3587418712c4ca8c86dee4f29c4c5f2a449b6d0a95a35bbb01ccc3e1a5c0b97d'
        assert cfg['base_inventory'] == [dict(path='decode_S256_B2_lm.rbln', bytes=15238278850,
            sha256='8919e9fea5c9fddc6f5941dbd80364790eeff1cc55b13c153c1a14991ee59332')]
        assert cfg['target_inventory'] == [dict(path='decode_S256_B8_lm.rbln', bytes=15228048882,
            sha256='304ff369a72c21afbbc84b7b09639503cd0e6327320a3ec23664d75e6c13460d')]
    assert cohort['status'] == 'succeeded' and cohort['valid'] is True
    assert cohort['stage'] == stage and cohort['traced'] is traced
    assert cohort['frozen_sha256'] == frozen_sha and cohort['arms'] == t.ARMS and cohort['order'] == t.orders(n)
    expected = [(block, arm) for block, order in enumerate(t.orders(n), 1) for arm in order]
    assert len(cohort['trials']) == len(expected)
    remote_root = PurePosixPath(cfg['driver']).parent.parent/(
        'trio88-stage-1e99504/actual-fixture' if fixture else 'trio88-model-1e99504')/label
    previous_end = None
    for row, (block, arm) in zip(cohort['trials'], expected):
        run_id = 'block-%02d-%s' % (block, arm)
        trial, remote = cohort_root/run_id, remote_root/run_id
        assert row['run_id'] == run_id and row['arm'] == arm
        assert {k: val for k, val in row.items() if k != 'run_id'} == json.loads((trial/'result.json').read_bytes())
        assert row['status'] == 'succeeded' and row['valid'] is row['treatment_valid'] is row['output_hash_equal'] is True
        assert row['output_inventory'] == cfg['target_inventory']
        assert row['physical_io'] is None
        assert json.loads((trial/'cleanup.json').read_bytes())['policy'] != 'retain'
        check_copies(row['base_copy_proof'], cfg['base_inventory'])
        check_copies(row['cache_copy_proof'], cfg['cache_inventory'])
        base = row['base_copy_proof'][0]
        assert row['donor_proof']['base_identity_before'] == [base['copy_dev'], base['copy_ino']]
        assert row['cli'] == json.loads((trial/'deploy.stdout').read_bytes())
        t.validate_treatment(cfg, arm, row['cli'], row['donor_proof'])
        command = row['command']
        assert command == json.loads((trial/'deploy.json').read_bytes()) and command['traced'] is traced
        v.check_command(command, t.argv(cfg, arm, remote), remote/'deploy.resources',
                        (trial/'deploy.resources').read_bytes().decode('utf-8'), t.h)
        assert command['environment'] == dict(GOMAXPROCS='32', GOMEMLIMIT=None, LC_ALL='C', TZ='UTC', TMPDIR=str(remote/'tmp'))
        assert command['elapsed_s'] == (command['monotonic_end_ns']-command['monotonic_start_ns'])/1e9
        assert 0 <= row['cli']['stats']['duration_ns']/1e9 <= command['elapsed_s']
        stamps = [command['monotonic_start_ns'], command['monotonic_end_ns'], row['sync_start_ns'], row['sync_end_ns'],
            row['validation_start_ns'], row['validation_end_ns'], row['donor_validation_start_ns'], row['donor_validation_end_ns']]
        assert stamps == sorted(stamps) and (previous_end is None or previous_end <= stamps[0])
        previous_end = stamps[-1]
        assert row['durable_s'] == (stamps[3]-stamps[0])/1e9
        assert row['validated_s'] == (stamps[5]-stamps[0])/1e9
        assert row['donor_validation_s'] == (stamps[7]-stamps[6])/1e9
        assert row['preparation_s'] >= 0 and row['preparation_inclusive_s'] >= row['validated_s']+row['preparation_s']-1e-6
        if traced:
            groups = v.check_trace(trial, remote, cfg, row, t.h)
            t.validate_io(arm, row['cli']['stats'], groups, sum(x['bytes'] for x in cfg['target_inventory']))
        else:
            assert row['logical_io'] is None and 'trace_accounting' not in row
            assert not list(trial.glob('deploy.trace.*'))
    summary = t.h.summarize(cohort['trials'], t.ARMS, n)
    assert cohort['summary'] == summary and all(x['valid'] for x in summary.values())
    if stage == 'smoke':
        t.check_smoke(frozen_sha, traced, cohort, cfg)
    logical = None
    if traced:
        logical = {a: {location: {metric: distribution([r['logical_io'][location][metric]
            for r in cohort['trials'] if r['arm'] == a]) for metric in cohort['trials'][0]['logical_io'][location]}
            for location in cohort['trials'][0]['logical_io']} for a in t.ARMS}
    return dict(valid=True, fixture=fixture, model_evidence=not fixture, label=label, trials=len(expected), n_per_arm=n,
        frozen_sha256=frozen_sha, driver_sha256=DRIVER_SHA, source_revision='1e99504a26c209a2dfba19d23c906b1899415ce3',
        timing={a: {key: distribution([r[key] if key != 'command_s' else r['command']['elapsed_s']
            for r in cohort['trials'] if r['arm'] == a]) for key in ['command_s', 'durable_s', 'validated_s',
                'donor_validation_s']} for a in t.ARMS},
        worker_reported_timing={a: {key: distribution([r[key] for r in cohort['trials'] if r['arm'] == a])
            for key in ['preparation_s', 'preparation_inclusive_s']} for a in t.ARMS},
        preparation_scope='Worker-reported, consistency-only: preparation start timestamp absent; not independently reconstructed',
        logical_io=logical, physical_io=None,
        scope='Exact source/command/output-hash/inode/clock witnesses and trace replay; absent model bytes not independently rehashed')


def verify(root, label, fixture=False, smoke_root=None):
    smoke = None
    if label.endswith('main'):
        assert smoke_root is not None, 'Main acceptance requires exact matching smoke archive'
        smoke = verify(smoke_root, 'io-smoke' if label.startswith('io-') else 'smoke', fixture)
    with tempfile.TemporaryDirectory(prefix='onggi-three-arm-') as directory:
        output = Path(directory)/'extracted'
        manifest = extract(root, output, label)
        result = verify_contents(output, label, fixture)
    result.update(archive_sha256=manifest['archive']['sha256'], files=len(manifest['files']))
    if smoke is not None:
        assert smoke['frozen_sha256'] == result['frozen_sha256'], 'Smoke/main starting conditions differ'
        result['excluded_smoke'] = dict(archive_sha256=smoke['archive_sha256'], trials=smoke['trials'],
                                       frozen_sha256=smoke['frozen_sha256'])
    result['control_provenance'] = 'Separate CPU Job/Pod UID cleanup, build and source receipts required'
    return result


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest='action', required=True)
    q = sub.add_parser('pack')
    q.add_argument('--source', type=Path, required=True)
    q.add_argument('--output', type=Path, required=True)
    q.add_argument('--label', choices=LABELS, required=True)
    q = sub.add_parser('verify')
    q.add_argument('--root', type=Path, required=True)
    q.add_argument('--label', choices=LABELS, required=True)
    q.add_argument('--fixture', action='store_true')
    q.add_argument('--smoke-root', type=Path)
    a = p.parse_args()
    result = pack(a.source, a.label, a.output) if a.action == 'pack' else verify(a.root, a.label, a.fixture, a.smoke_root)
    print(json.dumps(result, indent=2))
