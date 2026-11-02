"""CPU causal cohorts. Python 3.10+, stdlib only; no runtime changes."""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import platform
import re
import shutil
import socket
import statistics
import subprocess
import sys
import time

KINDS = {'packs', 'index', 'lists', 'manifests', 'config', 'refs'}
IO_CALLS = ('read,pread64,readv,preadv,preadv2,write,pwrite64,writev,pwritev,pwritev2,'
            'sendfile,copy_file_range,splice,mmap,msync,truncate,ftruncate,fsync,fdatasync,'
            'rename,renameat,renameat2,io_uring_setup,ioctl')


def save(path, value):
    pathlib.Path(path).write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')


def sha(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for block in iter(lambda: stream.read(8 << 20), b''):
            h.update(block)
    return h.hexdigest()


def inventory(root):
    root = pathlib.Path(root)
    if not root.is_dir() or root.is_symlink():
        raise ValueError('expected a real directory: ' + str(root))
    rows = []
    for p in sorted(root.rglob('*')):
        if p.is_symlink() or not (p.is_file() or p.is_dir()):
            raise ValueError('symlink/special file: ' + str(p))
        if p.is_file():
            rows.append({'path': p.relative_to(root).as_posix(), 'bytes': p.stat().st_size, 'sha256': sha(p)})
    return rows


def copy_base(source, destination):
    before = inventory(source)
    destination = pathlib.Path(destination)
    if destination.exists():
        raise FileExistsError(destination)
    # A real copy gives independent file ownership; reflinks/physical allocation are not inferred.
    shutil.copytree(source, destination)
    if inventory(destination) != before:
        raise ValueError('base copy SHA mismatch')
    proof = []
    for row in before:
        a, b = (pathlib.Path(source) / row['path']).stat(), (destination / row['path']).stat()
        independent = (a.st_dev, a.st_ino) != (b.st_dev, b.st_ino) and b.st_nlink == 1
        if not independent:
            raise ValueError('base copy shares inode or has multiple links')
        proof.append(dict(row, independent_inode=independent, source_dev=a.st_dev, source_ino=a.st_ino,
                          copy_dev=b.st_dev, copy_ino=b.st_ino, nlink=b.st_nlink,
                          allocated_bytes=getattr(b, 'st_blocks', None) * 512 if hasattr(b, 'st_blocks') else None))
    return proof


def trace_argv(prefix):
    return ['strace', '-ff', '-qq', '-yy', '-s', '0', '-o', str(prefix), '-e', 'trace=' + IO_CALLS]


def parse_trace(lines, roots):
    groups = {key: dict(read_bytes=0, write_bytes=0, errors=0, fsync_calls=0,
                        fdatasync_calls=0, ftruncate_calls=0, truncate_calls=0) for key in roots}
    warnings = []
    roots = {key: str(value).rstrip('/') for key, value in roots.items()}

    def group(path):
        path = path.removesuffix(' (deleted)')
        for key, root in sorted(roots.items(), key=lambda pair: -len(pair[1])):
            if path == root or path.startswith(root + '/'):
                return groups[key]
        return None

    pending = None
    for raw in lines:
        line = raw.strip()
        if '<unfinished ...>' in line:
            if pending is not None:
                warnings.append('overlapping unfinished syscall')
            pending = line.replace('<unfinished ...>', '')
            continue
        if line.startswith('<...') and ' resumed>' in line:
            if pending is None:
                warnings.append('orphan resumed syscall')
                continue
            line = pending + line.split(' resumed>', 1)[1]
            pending = None
        match = re.match(r'(\w+)\((.*)\)\s+=\s+(-?\d+|0x[0-9a-f]+)', line)
        if not match:
            if line and not line.startswith(('---', '+++')):
                warnings.append('unparsed: ' + line[:120])
            continue
        call, args, result = match.groups()
        n = int(result, 16 if result.startswith('0x') else 10)
        # -yy annotates each descriptor, including copies and inherited descriptors.
        paths = re.findall(r'\d+<([^>]*)>', args)
        if call == 'truncate':
            paths += re.findall(r'^"([^"]+)"', args)
        targets = [group(path) for path in paths]
        if call == 'io_uring_setup' and n >= 0:
            warnings.append('io_uring operations are not attributed')
            for item in groups.values():
                item['read_bytes'] = item['write_bytes'] = None
        if call == 'mmap' and n >= 0:
            for item in targets:
                if item is not None:
                    item['read_bytes'] = None
                    if 'PROT_WRITE' in args and 'MAP_SHARED' in args:
                        item['write_bytes'] = None
                    warnings.append('file-backed mmap: ' + line[:120])
            continue
        if call == 'ioctl' and n >= 0 and any(t is not None for t in targets) and not any(
                name in args for name in ['FIOCLEX', 'FIONCLEX', 'FIONBIO', 'FIONREAD', 'TCGETS']):
            warnings.append('file ioctl needs manual accounting: ' + line[:120])
        operations = []
        if call in {'read', 'pread64', 'readv', 'preadv', 'preadv2'}:
            operations = [(0, 'read_bytes')]
        elif call in {'write', 'pwrite64', 'writev', 'pwritev', 'pwritev2'}:
            operations = [(0, 'write_bytes')]
        elif call in {'copy_file_range', 'splice'}:
            operations = [(0, 'read_bytes'), (1, 'write_bytes')]
        elif call == 'sendfile':
            operations = [(0, 'write_bytes'), (1, 'read_bytes')]
        for index, counter in operations:
            item = targets[index] if index < len(targets) else None
            if item is not None:
                if n < 0:
                    item['errors'] += 1
                elif item[counter] is not None:
                    item[counter] += n
        if call in {'fsync', 'fdatasync', 'ftruncate', 'truncate'} and n >= 0:
            for item in targets:
                if item is not None:
                    item[call + '_calls'] += 1
    if pending is not None:
        warnings.append('incomplete syscall at trace end')
    return {'valid': not warnings, 'groups': groups, 'warnings': warnings,
            'scope': 'successful syscall return lengths; logical file I/O, not physical disk I/O'}


def read_traces(prefix, roots):
    paths = sorted(pathlib.Path(prefix).parent.glob(pathlib.Path(prefix).name + '.*'))
    if not paths:
        return {'valid': False, 'groups': {}, 'warnings': ['no per-thread traces']}
    parts = [parse_trace(p.read_text(errors='replace').splitlines(), roots) for p in paths]
    result = parts[0]
    for part in parts[1:]:
        result['valid'] &= part['valid']
        result['warnings'] += part['warnings']
        for key, row in part['groups'].items():
            for counter, n in row.items():
                old = result['groups'][key][counter]
                result['groups'][key][counter] = None if old is None or n is None else old + n
    result['trace_files'] = [p.name for p in paths]
    return result


def execute(argv, directory, label, env=None, trace=False):
    directory = pathlib.Path(directory)
    out, err = directory / (label + '.stdout'), directory / (label + '.stderr')
    actual = list(map(str, argv))
    if trace:
        actual = trace_argv(directory / (label + '.trace')) + actual
    resource_path = directory / (label + '.resources')
    if sys.platform == 'linux':
        actual = ['/usr/bin/time', '-v', '-o', str(resource_path)] + actual
    start = time.perf_counter_ns()
    record = dict(argv=list(map(str, argv)), executed_argv=actual, utc_start=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  monotonic_start_ns=start, status='running', process_io=None,
                  process_io_reason='no validated pre-exit per-process snapshot; unavailable', traced=trace)
    save(directory / (label + '.json'), record)
    try:
        with out.open('xb') as stdout, err.open('xb') as stderr:
            child = subprocess.run(actual, stdout=stdout, stderr=stderr, env=env)
        record['exit_code'] = child.returncode
        record['status'] = 'succeeded' if child.returncode == 0 else 'failed'
    except OSError as error:
        record.update(exit_code=None, status='failed', error=repr(error))
    record['monotonic_end_ns'] = time.perf_counter_ns()
    record['elapsed_s'] = (record['monotonic_end_ns'] - start) / 1e9
    if resource_path.exists():
        record['gnu_time'] = resource_path.read_text()
        record['resources'] = time_metrics(record['gnu_time'])
    save(directory / (label + '.json'), record)
    return record


def time_metrics(text):
    fields = [('user_s', 'User time (seconds)', float), ('system_s', 'System time (seconds)', float),
              ('max_rss_kib', 'Maximum resident set size (kbytes)', int),
              ('filesystem_input_blocks', 'File system inputs', int),
              ('filesystem_output_blocks', 'File system outputs', int)]
    result = {}
    for key, label, convert in fields:
        match = re.search(re.escape(label) + r':\s*([\d.]+)', text)
        result[key] = convert(match[1]) if match else None
    return result


def store_totals(root):
    root = pathlib.Path(root)
    kinds = {k: {'objects': 0, 'bytes': 0} for k in KINDS | {'tmp'}}
    objects = []
    for p in sorted(root.rglob('*')):
        if p.is_symlink():
            raise ValueError('store symlink: ' + str(p))
        if p.is_file():
            kind = p.relative_to(root).parts[0]
            if kind not in kinds:
                raise ValueError('unexpected store object: ' + str(p))
            n = p.stat().st_size
            kinds[kind]['objects'] += 1
            kinds[kind]['bytes'] += n
            objects.append({'path': p.relative_to(root).as_posix(), 'bytes': n})
    total = sum(kinds[k]['bytes'] for k in KINDS)
    return dict(kinds=kinds, objects=objects, pack_bytes=kinds['packs']['bytes'],
                metadata_bytes=total - kinds['packs']['bytes'], finalized_bytes=total,
                tmp_bytes=kinds['tmp']['bytes'])


def rsync_stats(text):
    result = {}
    for key, label in [('sent_bytes', 'sent'), ('received_bytes', 'received')]:
        match = re.search(r'Total bytes ' + label + r':\s*([\d,]+)', text)
        result[key] = int(match[1].replace(',', '')) if match else None
    result['protocol_bytes'] = sum(result.values()) if all(n is not None for n in result.values()) else None
    return result


def orders(arms, repetitions):
    if 'P' in arms and 'R' in arms:
        rest = [arm for arm in arms if arm not in {'P', 'R'}]
        return [(['P', 'R'] + rest if i % 2 == 0 else list(reversed(rest)) + ['R', 'P'])
                for i in range(repetitions)]
    return [arms[i % len(arms):] + arms[:i % len(arms)] for i in range(repetitions)]


def deploy_argv(cfg, arm, trial):
    trial = pathlib.Path(trial)
    output = (trial / 'output').as_posix()
    base = (trial / 'base').as_posix()
    if arm in {'P', 'R'}:
        argv = [cfg['onggi'], 'pull', '--store', cfg['store'], '--workers', str(cfg['workers']), '--json']
        if cfg.get('cache_dir'):
            argv += ['--cache', (trial / 'cache').as_posix()]
        if arm == 'R':
            argv += ['--base', cfg['base_manifest'], '--base-dir', base]
        return argv + [cfg['target_manifest'], output]
    if len(cfg['base_inventory']) != 1 or len(cfg['target_inventory']) != 1:
        raise ValueError('rsync/xdelta arms require the exact single-file fixture')
    target = cfg['target_inventory'][0]['path']
    if arm in {'rsync', 'rsync_inplace'}:
        return (['rsync', '--no-whole-file', '--ignore-times', '--stats'] +
                (['--inplace'] if arm == 'rsync_inplace' else []) +
                [(pathlib.Path(cfg['target_dir']) / target).as_posix(), output + '/'])
    if arm == 'xdelta':
        return ['xdelta3', '-d', '-s', (trial / 'base' / cfg['base_inventory'][0]['path']).as_posix(),
                cfg['delta_file'], (trial / 'output' / target).as_posix()]
    raise ValueError('unknown deployment arm: ' + arm)


def publish_argv(cfg, arm, trial, ref, source):
    if arm not in {'C', 'F', 'T', 'TF', 'TA'}:
        raise ValueError('unknown publication arm')
    fine = arm == 'F'
    two = arm in {'T', 'TF', 'TA'}
    return [cfg['onggi'], 'push', '--store', (trial / 'store').as_posix(),
            '--min', '1KiB' if fine else '16KiB', '--avg', '4KiB' if fine else '64KiB',
            '--max', '16KiB' if fine else '256KiB', '--sub', '4KiB' if two else '0',
            '--sub-kinds', 'atom-prefill,atom-decode,atom-other',
            '--frame', '256KiB' if arm in {'TF', 'TA'} else '0',
            '--compress', 'auto' if arm == 'TA' else 'zstd', '--pack-size', '64MiB',
            '--workers', str(cfg['workers']), '--uploads', '4', '--quiet', '--json',
            '--tmp', (trial / 'tmp').as_posix(), '--ref', ref, str(source)]


def host_info():
    info = dict(hostname=socket.gethostname(), platform=platform.platform(), python=sys.version,
                cpu_count=os.cpu_count(), affinity=sorted(os.sched_getaffinity(0)) if hasattr(os, 'sched_getaffinity') else None,
                load=os.getloadavg() if hasattr(os, 'getloadavg') else None)
    for name in ['/proc/self/mountinfo', '/sys/fs/cgroup/cpu.max', '/sys/fs/cgroup/memory.max']:
        p = pathlib.Path(name)
        info[name] = p.read_text() if p.exists() else None
    return info


def ensure_disjoint(output, cfg):
    output = pathlib.Path(output).resolve()
    for key in ['base_dir', 'target_dir', 'store', 'cache_dir']:
        if cfg.get(key):
            source = pathlib.Path(cfg[key]).resolve()
            if output == source or output in source.parents or source in output.parents:
                raise ValueError('run root overlaps immutable ' + key)


def freeze(config_path, output):
    cfg = json.loads(pathlib.Path(config_path).read_text())
    if cfg.get('status') != 'not_run':
        raise ValueError('configuration must start not_run')
    if cfg.get('workers') != 8 or cfg.get('gomaxprocs') != 32:
        raise ValueError('primary CPU protocol fixes workers=8 and GOMAXPROCS=32')
    if not cfg.get('source_revision') or not cfg.get('binary_sha256') or not cfg.get('limits_description'):
        raise ValueError('record binary/source identities and actual host/worker limits before freezing')
    for key in ['onggi', 'base_dir', 'target_dir', 'store', 'cache_dir']:
        if cfg.get(key):
            cfg[key] = str(pathlib.Path(cfg[key]).resolve(strict=True))
    if sha(cfg['onggi']) != cfg['binary_sha256']:
        raise ValueError('binary SHA mismatch')
    cfg['base_inventory'] = inventory(cfg['base_dir'])
    cfg['target_inventory'] = inventory(cfg['target_dir'])
    if not cfg['base_inventory'] or not cfg['target_inventory']:
        raise ValueError('empty input inventory')
    if cfg.get('store'):
        for key in ['base_manifest', 'target_manifest']:
            if not re.fullmatch(r'sha256:[0-9a-f]{64}', cfg[key]):
                raise ValueError('use immutable sha256 manifest digests')
        cfg['store_inventory'] = inventory(cfg['store'])
    if cfg.get('cache_dir'):
        cfg['cache_inventory'] = inventory(cfg['cache_dir'])
    cfg.update(host=host_info(), frozen_utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),
               harness_sha256=sha(__file__), config_source_sha256=sha(config_path),
               conditions=dict(chunk_cache='identical frozen copies' if cfg.get('cache_dir') else 'disabled',
                               peers='disabled', page_cache='warm: rehash both immutable inputs before each arm; no host cache changes',
                               repetition_count=5, smoke_excluded=True, tracing='separate diagnostic cohort',
                               clocks='command; final file sync; identical output SHA; preparation separately',
                               physical_io='unavailable', process_io='unavailable: no validated pre-exit snapshot'))
    output = pathlib.Path(output)
    if output.exists():
        raise FileExistsError(output)
    save(output, cfg)
    return cfg


def sync_tree(root):
    count = 0
    for p in sorted(pathlib.Path(root).rglob('*')):
        if p.is_file():
            with p.open('rb') as stream:
                os.fsync(stream.fileno())
            count += 1
    return count


def checked(row):
    if row['status'] != 'succeeded':
        raise RuntimeError('command failed; retained raw logs')
    return row


def check_frozen(cfg):
    if sha(__file__) != cfg['harness_sha256'] or sha(cfg['onggi']) != cfg['binary_sha256']:
        raise ValueError('harness or binary changed after freeze')
    for key in ['base', 'target', 'store', 'cache']:
        path_key = key + '_dir' if key != 'store' else key
        if cfg.get(path_key) and inventory(cfg[path_key]) != cfg[key + '_inventory']:
            raise ValueError('frozen inventory changed: ' + key)


def command(cfg, argv, trial, label, trace=False):
    (trial / 'tmp').mkdir(exist_ok=True)
    env = dict(os.environ, GOMAXPROCS=str(cfg['gomaxprocs']), LC_ALL='C', TZ='UTC', TMPDIR=str(trial / 'tmp'))
    # Only this explicit whitelist is written to records; inherited secrets are never logged.
    row = execute(['nice', '-n', '19'] + argv, trial, label, env=env, trace=trace)
    row['environment'] = {key: env.get(key) for key in ['GOMAXPROCS', 'GOMEMLIMIT', 'LC_ALL', 'TZ', 'TMPDIR']}
    save(trial / (label + '.json'), row)
    return row


def trace_roots(cfg, trial):
    roots = dict(target=(trial / 'output').as_posix(), base=(trial / 'base').as_posix(), store=cfg['store'],
                 source_base=cfg['base_dir'], source_target=cfg['target_dir'], staging=(trial / 'tmp').as_posix())
    if cfg.get('cache_dir'):
        roots['cache'] = (trial / 'cache').as_posix()
    if cfg.get('delta_file'):
        roots['delta'] = cfg['delta_file']
    return roots


def deploy_trial(cfg, arm, trial, trace):
    trial.mkdir()
    result = dict(arm=arm, status='running', valid=False, logical_io=None, publication='not applicable: prepublished source')
    save(trial / 'result.json', result)
    try:
        preparation_start = start = time.perf_counter_ns()
        if inventory(cfg['base_dir']) != cfg['base_inventory'] or inventory(cfg['target_dir']) != cfg['target_inventory']:
            raise ValueError('immutable source changed')
        result['warm_source_verification_s'] = (time.perf_counter_ns() - start) / 1e9
        start = time.perf_counter_ns()
        result['base_copy_proof'] = copy_base(cfg['base_dir'], trial / 'base')
        if cfg.get('cache_dir'):
            copy_base(cfg['cache_dir'], trial / 'cache')
            if inventory(trial / 'cache') != cfg['cache_inventory']:
                raise ValueError('cache differs from frozen inventory')
        output = trial / 'output'
        if arm in {'rsync', 'rsync_inplace', 'xdelta'}:
            if len(cfg['base_inventory']) != 1 or len(cfg['target_inventory']) != 1:
                raise ValueError('baseline requires one file per artifact')
            target = output / cfg['target_inventory'][0]['path']
            target.parent.mkdir(parents=True)
            if arm.startswith('rsync'):
                (trial / 'base' / cfg['base_inventory'][0]['path']).rename(target)
        result['preparation_s'] = (time.perf_counter_ns() - start) / 1e9
        result['free_bytes_before_command'] = shutil.disk_usage(trial).free
        result['command'] = command(cfg, deploy_argv(cfg, arm, trial), trial, 'deploy', trace)
        checked(result['command'])
        result['sync_start_ns'] = start = time.perf_counter_ns()
        result['synced_files'] = sync_tree(output)
        result['sync_end_ns'] = time.perf_counter_ns()
        result['sync_s'] = (result['sync_end_ns'] - start) / 1e9
        result['validation_start_ns'] = start = time.perf_counter_ns()
        result['output_inventory'] = inventory(output)
        result['validation_end_ns'] = time.perf_counter_ns()
        result['validation_s'] = (result['validation_end_ns'] - start) / 1e9
        result['output_hash_equal'] = result['output_inventory'] == cfg['target_inventory']
        if arm in {'P', 'R'}:
            result['cli'] = json.loads((trial / 'deploy.stdout').read_text())
            stats = result['cli']['stats']
            result['consume_treatment_valid'] = stats.get('base') == cfg['base_manifest'] if arm == 'R' else not stats.get('base')
            result['cli_bytes_scope'] = 'logical target size; never observed file writes'
        else:
            result['consume_treatment_valid'] = True
            if arm.startswith('rsync'):
                result['rsync_protocol'] = rsync_stats((trial / 'deploy.stdout').read_text())
        result['valid'] = result['output_hash_equal'] and result['consume_treatment_valid']
        result['durable_s'] = (result['sync_end_ns'] - result['command']['monotonic_start_ns']) / 1e9
        result['validated_s'] = (result['validation_end_ns'] - result['command']['monotonic_start_ns']) / 1e9
        result['preparation_inclusive_s'] = (result['validation_end_ns'] - preparation_start) / 1e9
        result['status'] = 'succeeded'
    except Exception as error:
        result.update(status='failed', error=repr(error))
    if trace and 'command' in result:
        result['trace_accounting'] = read_traces(trial / 'deploy.trace', trace_roots(cfg, trial))
        if result['trace_accounting']['valid']:
            result['logical_io'] = result['trace_accounting']['groups']
        else:
            result['valid'] = False
    result['retained_artifact_bytes'] = sum(p.stat().st_size for p in trial.rglob('*') if p.is_file())
    save(trial / 'result.json', result)
    return result


def summarize(rows, arms, expected, cohort_valid=True):
    def distribution(values):
        quartiles = statistics.quantiles(values, method='inclusive') if len(values) > 1 else [values[0]] * 3
        return dict(values=values, median=statistics.median(values), q1=quartiles[0], q3=quartiles[2])
    result = {}
    for arm in arms:
        samples = [row for row in rows if row['arm'] == arm]
        valid = cohort_valid and len(samples) == expected and all(row['status'] == 'succeeded' and row['valid'] for row in samples)
        row = dict(samples=samples, valid=valid, command_s=None, validated_s=None)
        if valid and all('command' in sample for sample in samples):
            row['command_s'] = distribution([sample['command']['elapsed_s'] for sample in samples])
            row['validated_s'] = distribution([sample['validated_s'] for sample in samples])
            for key in ['user_s', 'system_s', 'max_rss_kib']:
                values = [sample['command'].get('resources', {}).get(key) for sample in samples]
                row[key] = distribution(values) if all(n is not None for n in values) else None
        if valid and all('b_publication_s' in sample for sample in samples):
            for key in ['b_publication_s', 'family_publication_s']:
                row[key] = distribution([sample[key] for sample in samples])
        result[arm] = row
    return result


def cleanup_trial(trial, retain):
    receipt = dict(policy='retain' if retain else 'remove successful trial artifact trees; retain raw evidence', removed=[])
    start = time.perf_counter_ns()
    if not retain:
        for name in ['base', 'output', 'cache', 'store', 'tmp', 'verify-a', 'verify-b']:
            path = trial / name
            if path.exists():
                if path.is_symlink() or trial.resolve() not in path.resolve().parents:
                    raise ValueError('unsafe cleanup path')
                shutil.rmtree(path)
                receipt['removed'].append(str(path))
    receipt['elapsed_s'] = (time.perf_counter_ns() - start) / 1e9
    save(trial / 'cleanup.json', receipt)


def run_matrix(frozen_path, output, experiment, stage, smoke_path=None, trace=False):
    if sys.platform != 'linux':
        raise ValueError('actual cohorts require Linux; no remote launch is performed by this script')
    cfg = json.loads(pathlib.Path(frozen_path).read_text())
    if trace and experiment != 'deploy':
        raise ValueError('tracing is a separate deployment diagnostic cohort')
    arms = cfg['deployment_arms'] if experiment == 'deploy' else cfg['publication_arms']
    if len(arms) != len(set(arms)) or not arms:
        raise ValueError('arms must be nonempty and unique')
    allowed = {'P', 'R', 'rsync', 'rsync_inplace', 'xdelta'} if experiment == 'deploy' else {'C', 'F', 'T', 'TF', 'TA'}
    if not set(arms) <= allowed:
        raise ValueError('unknown arm')
    frozen_sha = sha(frozen_path)
    if stage == 'main':
        if not smoke_path:
            raise ValueError('main requires separate actual-pair smoke cohort.json')
        smoke = json.loads(pathlib.Path(smoke_path).read_text())
        if not (smoke.get('valid') and smoke.get('stage') == 'smoke' and smoke.get('frozen_sha256') == frozen_sha
                and smoke.get('experiment') == experiment and smoke.get('traced') == trace and smoke.get('arms') == arms):
            raise ValueError('smoke does not match frozen conditions/cohort')
    ensure_disjoint(output, cfg)
    check_frozen(cfg)
    output = pathlib.Path(output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    shutil.copyfile(frozen_path, output / 'frozen.json')
    shutil.copyfile(__file__, output / 'causal.py')
    repetitions = 5 if stage == 'main' else 1
    cohort = dict(status='running', valid=False, experiment=experiment, stage=stage, traced=trace,
                  frozen_sha256=frozen_sha, arms=arms, order=orders(arms, repetitions), trials=[],
                  host=host_info(), free_bytes_start=shutil.disk_usage(output).free,
                  io_scope='separate traced syscall diagnostic' if trace else 'untraced timing; logical I/O unavailable',
                  physical_io=None, process_io=None)
    save(output / 'cohort.json', cohort)
    try:
        tools = [(cfg['onggi'], ['version']), ('nice', ['--version']), ('/usr/bin/time', ['--version'])]
        if trace:
            tools += [('strace', ['--version'])]
        if any(a.startswith('rsync') for a in arms):
            tools += [('rsync', ['--version']), ('rsync', ['--help'])]
        if 'xdelta' in arms:
            tools += [('xdelta3', ['-V'])]
        for index, (tool, args) in enumerate(tools):
            checked(execute([tool] + args, output, 'tool-' + str(index)))
            if tool == 'rsync' and args == ['--help'] and '--inplace' not in (output / ('tool-' + str(index) + '.stdout')).read_text():
                raise ValueError('rsync lacks --inplace')
        if 'xdelta' in arms:
            if len(cfg['base_inventory']) != 1 or len(cfg['target_inventory']) != 1:
                raise ValueError('xdelta fixture must contain one file each')
            cfg['delta_file'] = str(output / 'delta.vcdiff')
            cohort['xdelta_publication'] = checked(command(cfg, ['xdelta3', '-e', '-s',
                str(pathlib.Path(cfg['base_dir']) / cfg['base_inventory'][0]['path']),
                str(pathlib.Path(cfg['target_dir']) / cfg['target_inventory'][0]['path']), cfg['delta_file']], output, 'encode-xdelta'))
            cohort['xdelta_delta_sha256'] = sha(cfg['delta_file'])
            cohort['xdelta_delta_bytes'] = pathlib.Path(cfg['delta_file']).stat().st_size
        for block, order in enumerate(cohort['order'], 1):
            for arm in order:
                trial = output / ('block-%02d-%s' % (block, arm))
                row = deploy_trial(cfg, arm, trial, trace) if experiment == 'deploy' else publication_trial(cfg, arm, trial)
                row['run_id'] = trial.name
                cohort['trials'].append(row)
                save(output / 'cohort.json', cohort)
                if row['status'] == 'succeeded' and row['valid']:
                    cleanup_trial(trial, cfg.get('retain_outputs', False))
                else:
                    raise RuntimeError('failed/invalid trial retained; dependent matrix stopped, no automatic retry')
        check_frozen(cfg)
        cohort['summary'] = summarize(cohort['trials'], arms, repetitions)
        cohort['valid'] = all(row['valid'] for row in cohort['summary'].values())
        cohort['status'] = 'succeeded'
    except Exception as error:
        cohort.update(status='failed', error=repr(error))
        cohort['summary'] = summarize(cohort['trials'], arms, repetitions, cohort_valid=False)
    cohort['free_bytes_end'] = shutil.disk_usage(output).free
    cohort['cleanup_policy'] = 'outputs retained' if cfg.get('retain_outputs') else 'successful artifact trees removed; failures and evidence retained'
    save(output / 'cohort.json', cohort)
    return cohort


def fixture(binary, root):
    """Actual small Onggi CLI gate; this is test data, never a model measurement."""
    import random
    root = pathlib.Path(root).resolve()
    for name in ['source-a', 'source-b']:
        (root / name).mkdir()
    raw = random.Random(17).randbytes(4 << 20) * 2
    changed = bytearray(raw)
    changed[3 << 20:(3 << 20) + (64 << 10)] = random.Random(18).randbytes(64 << 10)
    (root / 'source-a/decode_S256_B2_lm.rbln').write_bytes(raw)
    (root / 'source-b/decode_S256_B8_lm.rbln').write_bytes(changed)
    cfg = dict(onggi=str(binary), workers=8, gomaxprocs=32, store=str(root / 'store'),
               base_dir=str(root / 'source-a'), target_dir=str(root / 'source-b'),
               base_inventory=inventory(root / 'source-a'), target_inventory=inventory(root / 'source-b'))
    for ref, name in [('a', 'source-a'), ('b', 'source-b')]:
        argv = [str(binary), 'push', '--store', cfg['store'], '--ref', ref, '--json', '--quiet', str(root / name)]
        checked(command(cfg, argv, root, 'push-' + ref))
        cfg['base_manifest' if ref == 'a' else 'target_manifest'] = json.loads((root / ('push-' + ref + '.stdout')).read_text())['manifest']
    plain = deploy_trial(cfg, 'P', root / 'plain', True)
    reuse = deploy_trial(cfg, 'R', root / 'reuse', True)
    valid = all(row['valid'] and row.get('trace_accounting', {}).get('valid') for row in [plain, reuse])
    result = dict(valid=valid, plain=plain, reuse=reuse,
                  plain_write_bytes=plain['logical_io']['target']['write_bytes'] if plain['logical_io'] else None,
                  consume_write_bytes=reuse['logical_io']['target']['write_bytes'] if reuse['logical_io'] else None)
    cfg['cache_dir'] = str(root / 'cache-source')
    checked(command(cfg, [str(binary), 'pull', '--store', cfg['store'], '--cache', cfg['cache_dir'],
                          '--json', cfg['base_manifest'], str(root / 'cache-seed-output')], root, 'seed-base-cache'))
    cfg['cache_inventory'] = inventory(cfg['cache_dir'])
    cached_plain = deploy_trial(cfg, 'P', root / 'cached-plain', True)
    cached_reuse = deploy_trial(cfg, 'R', root / 'cached-reuse', True)
    result['cached'] = dict(plain=cached_plain, reuse=cached_reuse,
                            valid=all(row['valid'] for row in [cached_plain, cached_reuse]))
    result['valid'] &= result['cached']['valid']
    save(root / 'fixture-result.json', result)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    p = sub.add_parser('freeze')
    p.add_argument('--config', required=True)
    p.add_argument('--output', required=True)
    p = sub.add_parser('run')
    p.add_argument('--frozen', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--experiment', choices=['deploy', 'publish'], required=True)
    p.add_argument('--stage', choices=['smoke', 'main'], required=True)
    p.add_argument('--smoke')
    p.add_argument('--trace', action='store_true')
    p = sub.add_parser('fixture')
    p.add_argument('--onggi', required=True)
    p.add_argument('--output', required=True)
    args = parser.parse_args()
    if args.action == 'freeze':
        freeze(args.config, args.output)
        return 0
    if args.action == 'fixture':
        root = pathlib.Path(args.output)
        root.mkdir(parents=True, exist_ok=False)
        result = fixture(pathlib.Path(args.onggi).resolve(strict=True), root)
    else:
        result = run_matrix(args.frozen, args.output, args.experiment, args.stage, args.smoke, args.trace)
    return 0 if result['valid'] else 1


def publication_trial(cfg, arm, trial):
    trial.mkdir()
    (trial / 'tmp').mkdir()
    result = dict(arm=arm, status='running', valid=False)
    save(trial / 'result.json', result)
    try:
        for key in ['base', 'target']:
            if inventory(cfg[key + '_dir']) != cfg[key + '_inventory']:
                raise ValueError('immutable publication input changed')
        for ref, key in [('a', 'base'), ('b', 'target')]:
            result['push_' + ref] = checked(command(cfg, publish_argv(cfg, arm, trial, ref, cfg[key + '_dir']), trial, 'push-' + ref))
            result['after_' + ref] = store_totals(trial / 'store')
            for name, args in [('apparent', ['-sb']), ('allocated', ['-sB1'])]:
                result['du_' + ref + '_' + name] = command(cfg, ['du'] + args + [str(trial / 'store')], trial, 'du-' + ref + '-' + name)
        result['b_publication_s'] = result['push_b']['elapsed_s']
        result['family_publication_s'] = result['push_a']['elapsed_s'] + result['push_b']['elapsed_s']
        result['b_object_delta'] = {key: result['after_b'][key] - result['after_a'][key]
                                    for key in ['pack_bytes', 'metadata_bytes', 'finalized_bytes']}
        configs = list((trial / 'store/config').rglob('*'))
        configs = [p for p in configs if p.is_file()]
        if len(configs) != 1:
            raise ValueError('expected exactly one actual stored configuration')
        result['stored_config'] = json.loads(configs[0].read_text())
        expected_frame = 256 << 10 if arm in {'TF', 'TA'} else 0
        if result['stored_config'].get('frame', 0) != expected_frame:
            raise ValueError('actual stored frame config differs from treatment')
        result['diff'] = checked(command(cfg, [cfg['onggi'], 'diff', '--store', str(trial / 'store'),
                                               '--json', '--ranges', 'a', 'b'], trial, 'diff'))
        for ref, key in [('a', 'base'), ('b', 'target')]:
            checked(command(cfg, [cfg['onggi'], 'pull', '--store', str(trial / 'store'), '--workers', str(cfg['workers']),
                                  '--json', ref, str(trial / ('verify-' + ref))], trial, 'verify-' + ref))
            if inventory(trial / ('verify-' + ref)) != cfg[key + '_inventory']:
                raise ValueError('publication roundtrip SHA mismatch')
        result.update(status='succeeded', valid=True)
    except Exception as error:
        result.update(status='failed', error=repr(error))
    save(trial / 'result.json', result)
    return result


if __name__ == '__main__':
    sys.exit(main())
