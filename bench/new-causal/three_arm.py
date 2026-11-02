"""Separate equal-content P/C/R cohort; historical causal.py stays unchanged."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import sys
import time
import types

# Execute exactly the reviewed source bytes, never a timestamp-valid cached pyc.
_helper = Path(__file__).with_name('causal.py')
_source = _helper.read_bytes()
if hashlib.sha256(_source).hexdigest() != '1546801671fc19d3871487b2d2d4f0f8e445bae82cbfd25aabf9e87351554f5a':
    raise ValueError('Unexpected historical helper source')
h = types.ModuleType('three_arm_causal_source')
h.__file__ = str(_helper)
exec(compile(_source, str(_helper), 'exec'), h.__dict__)

ARMS = ['P', 'C', 'R']


def require(condition, message):
    if not condition:
        raise ValueError(message)


def orders(n):
    require(type(n) is int and n in (1, 5), 'Only excluded smoke1 or main5 blocks are defined')
    return [list(x) for x in ['PCR', 'RCP', 'CPR', 'RPC', 'PRC']][:n]


def validate_treatment(cfg, arm, report, proof):
    require(arm in ARMS and report.get('policy') == arm and report.get('manifest') == cfg['target_manifest'],
            'Wrong treatment or target identity')
    s = report.get('stats', {})
    for key in ['read_bytes', 'peer_bytes', 'cached', 'reused', 'local_bytes']:
        require(type(s.get(key)) is int and s[key] >= 0, 'Missing/invalid actual counter: '+key)
    require(s['read_bytes'] == s['peer_bytes'] == s['cached'] == 0, 'Complete cache did not supply residual payload')
    require(type(report.get('claim_calls')) is int and report['claim_calls'] == (0 if arm == 'P' else 1),
            'Expected claim/refusal was not witnessed')
    require(report.get('admitted') is (arm == 'R'), 'Wrong admission outcome')
    if arm == 'R':
        require(s.get('base') == cfg['base_manifest'] and s['reused'] > 0 and s['local_bytes'] == 0
                and not s.get('local_base'), 'R did not actually consume the base')
        require(proof['base_inventory_after'] == [] and proof['base_identity_after'] is None
                and proof['target_identity'] == proof['base_identity_before'], 'R did not transfer the original inode')
    else:
        require(not s.get('base') and s['reused'] == 0, 'Intact arm consumed a base')
        require(proof['base_inventory_after'] == cfg['base_inventory']
                and proof['base_identity_after'] == proof['base_identity_before']
                and proof['target_identity'] != proof['base_identity_before'], 'Intact donor bytes/inode changed or aliased output')
        if arm == 'C':
            require(s.get('local_base') == cfg['base_manifest'] and s['local_bytes'] > 0, 'C fell back without actual intact-base copying')
        else:
            require(not s.get('local_base') and s['local_bytes'] == 0, 'P used a local base')
    return True


def check_smoke(frozen_sha, traced, smoke, cfg=None):
    require(smoke.get('stage') == 'smoke' and smoke.get('status') == 'succeeded'
            and smoke.get('valid') is True and smoke.get('traced') is traced
            and smoke.get('frozen_sha256') == frozen_sha and smoke.get('arms') == ARMS,
            'Main requires the matching successful complete three-arm smoke')
    require(cfg is not None and smoke.get('order') == orders(1), 'Smoke configuration/order missing or changed')
    rows = smoke.get('trials', [])
    require(len(rows) == 3 and [r.get('arm') for r in rows] == ARMS
            and [r.get('run_id') for r in rows] == ['block-01-'+a for a in ARMS], 'Smoke attempts missing or reordered')
    for row in rows:
        require(row.get('status') == 'succeeded' and row.get('valid') is True
                and row.get('treatment_valid') is True and row.get('output_hash_equal') is True
                and row.get('output_inventory') == cfg['target_inventory'], 'Incomplete smoke output/treatment witness')
        command = row.get('command', {})
        require(command.get('status') == 'succeeded' and command.get('exit_code') == 0
                and command.get('traced') is traced, 'Smoke command failed or has wrong instrumentation')
        validate_treatment(cfg, row['arm'], row.get('cli', {}), row.get('donor_proof', {}))
        if traced:
            accounting = row.get('trace_accounting', {})
            require(accounting.get('valid') is True and not accounting.get('warnings')
                    and accounting.get('trace_files') and row.get('logical_io') == accounting.get('groups'),
                    'Missing smoke trace attribution')
            validate_io(row['arm'], row['cli']['stats'], accounting['groups'],
                        sum(f['bytes'] for f in cfg['target_inventory']))
    require(smoke.get('summary') == h.summarize(rows, ARMS, 1), 'Smoke summary inconsistent with actual attempts')
    return True


def validate_io(arm, stats, groups, target_size):
    require(all(g['errors'] == 0 for g in groups.values()), 'Artifact syscall errors require manual interpretation')
    for source in ['source_base', 'source_target']:
        require(groups[source]['read_bytes'] == groups[source]['write_bytes'] == 0, 'Command accessed immutable source')
    require(groups['store']['write_bytes'] == groups['base']['write_bytes'] == 0,
            'Source/base writes outside admitted target')
    require(groups['base']['read_bytes'] >= stats['local_bytes'] if arm == 'C'
            else groups['base']['read_bytes'] == 0, 'Donor reads do not witness the intended treatment')
    writes = groups['target']['write_bytes']
    require(writes >= target_size if arm in ['P', 'C'] else 0 < writes < target_size,
            'Missing full/changed target writes')
    require(groups['target']['fsync_calls'] >= 1, 'Missing command target sync')
    return True


def check_extra_sources(cfg):
    for key, path in [('driver_sha256', cfg['driver']), ('driver_source_sha256', cfg['driver_source']),
                      ('three_arm_sha256', __file__)]:
        require(h.sha(path) == cfg[key], 'Comparison source/binary changed: '+key)
    return True


def argv(cfg, arm, trial):
    require(arm in ARMS, 'Unknown materialization policy')
    return [cfg['driver'], '--store', cfg['store'], '--cache', str(trial/'cache'),
            '--base', cfg['base_manifest'], '--base-dir', str(trial/'base'),
            '--workers', str(cfg['workers']), '--policy', arm,
            cfg['target_manifest'], str(trial/'output')]


def identity(path):
    if not path.exists():
        return None
    stat = path.stat()
    return [stat.st_dev, stat.st_ino]


def trial(cfg, arm, directory, traced):
    directory = Path(directory)
    directory.mkdir()
    row = dict(arm=arm, status='running', valid=False, logical_io=None, physical_io=None)
    h.save(directory/'result.json', row)
    preparation_start = time.perf_counter_ns()
    try:
        require(len(cfg['base_inventory']) == len(cfg['target_inventory']) == 1, 'This protocol requires one file per artifact')
        start = time.perf_counter_ns()
        require(h.inventory(cfg['base_dir']) == cfg['base_inventory']
                and h.inventory(cfg['target_dir']) == cfg['target_inventory'], 'Immutable input changed')
        row['warm_source_verification_s'] = (time.perf_counter_ns()-start)/1e9
        row['base_copy_proof'] = h.copy_base(cfg['base_dir'], directory/'base')
        row['cache_copy_proof'] = h.copy_base(cfg['cache_dir'], directory/'cache')
        require(h.inventory(directory/'cache') == cfg['cache_inventory'], 'Cache inventory differs')
        row['preparation_s'] = (time.perf_counter_ns()-preparation_start)/1e9
        row['command'] = h.command(cfg, argv(cfg, arm, directory), directory, 'deploy', traced)
        h.checked(row['command'])
        row['sync_start_ns'] = time.perf_counter_ns()
        row['synced_files'] = h.sync_tree(directory/'output')
        row['sync_end_ns'] = time.perf_counter_ns()
        row['validation_start_ns'] = time.perf_counter_ns()
        row['output_inventory'] = h.inventory(directory/'output')
        row['validation_end_ns'] = time.perf_counter_ns()
        row['output_hash_equal'] = row['output_inventory'] == cfg['target_inventory']
        row['cli'] = json.loads((directory/'deploy.stdout').read_bytes())
        row['donor_validation_start_ns'] = time.perf_counter_ns()
        proof = row['donor_proof'] = dict(
            base_identity_before=[row['base_copy_proof'][0]['copy_dev'], row['base_copy_proof'][0]['copy_ino']],
            base_inventory_after=h.inventory(directory/'base'),
            base_identity_after=identity(directory/'base'/cfg['base_inventory'][0]['path']),
            target_identity=identity(directory/'output'/cfg['target_inventory'][0]['path']))
        row['donor_validation_end_ns'] = time.perf_counter_ns()
        row['treatment_valid'] = validate_treatment(cfg, arm, row['cli'], proof)
        require(row['output_hash_equal'], 'Target SHA/inventory mismatch')
        row['durable_s'] = (row['sync_end_ns']-row['command']['monotonic_start_ns'])/1e9
        row['validated_s'] = (row['validation_end_ns']-row['command']['monotonic_start_ns'])/1e9
        row['preparation_inclusive_s'] = (row['validation_end_ns']-preparation_start)/1e9
        row['donor_validation_s'] = (row['donor_validation_end_ns']-row['donor_validation_start_ns'])/1e9
        if traced:
            accounting = row['trace_accounting'] = h.read_traces(directory/'deploy.trace', h.trace_roots(cfg, directory))
            require(accounting['valid'] and not accounting['warnings'] and accounting['trace_files'], 'Incomplete trace attribution')
            row['logical_io'] = groups = accounting['groups']
            validate_io(arm, row['cli']['stats'], groups, sum(f['bytes'] for f in cfg['target_inventory']))
        row.update(status='succeeded', valid=True)
    except Exception as error:
        row.update(status='failed', valid=False, error=repr(error))
    row['retained_artifact_bytes'] = sum(p.stat().st_size for p in directory.rglob('*') if p.is_file())
    h.save(directory/'result.json', row)
    return row


def prepare(legacy, driver, output):
    require(h.sha(legacy) == '75b2df992e3369f94cb6424c422432dc2155ef9e9af76331fe3eaca5eb563f09', 'Unexpected legacy cache/input conditions')
    cfg = json.loads(Path(legacy).read_bytes())
    h.check_frozen(cfg)
    driver = Path(driver).resolve(strict=True)
    cfg.update(status='not_run', driver=str(driver), driver_sha256=h.sha(driver),
               driver_source=str(driver.parent/'three_arm_call.go'),
               driver_source_sha256=h.sha(driver.parent/'three_arm_call.go'),
               three_arm_sha256=h.sha(__file__), deployment_arms=ARMS)
    output = Path(output).resolve()
    h.ensure_disjoint(output, cfg)
    output.mkdir(exist_ok=False)
    h.save(output/'config.json', cfg)
    frozen = h.freeze(output/'config.json', output/'frozen.json')
    check_extra_sources(frozen)
    return frozen


def fixture(binary, driver, output):
    """Actual CLI/driver/trace integration, synthetic bytes; never model evidence."""
    import random
    require(sys.platform == 'linux', 'Actual integration fixture requires Linux')
    binary, driver = Path(binary).resolve(strict=True), Path(driver).resolve(strict=True)
    root = Path(output).resolve()
    root.mkdir(exist_ok=False)
    for name in ['source-a', 'source-b']:
        (root/name).mkdir()
    raw = random.Random(17).randbytes(4 << 20)*2
    changed = bytearray(raw)
    changed[3 << 20:(3 << 20)+(64 << 10)] = random.Random(18).randbytes(64 << 10)
    (root/'source-a/decode_S256_B2_lm.rbln').write_bytes(raw)
    (root/'source-b/decode_S256_B8_lm.rbln').write_bytes(changed)
    cfg = dict(status='not_run', onggi=str(binary), binary_sha256=h.sha(binary),
               source_revision='fixture core05c9760; synthetic opaque bytes, not model performance',
               limits_description='CPU-only integration fixture; actual cgroups recorded by freeze',
               workers=8, gomaxprocs=32, store=str(root/'store'),
               base_dir=str(root/'source-a'), target_dir=str(root/'source-b'), cache_dir=str(root/'complete-cache'),
               driver=str(driver), driver_sha256=h.sha(driver), driver_source=str(driver.parent/'three_arm_call.go'),
               driver_source_sha256=h.sha(driver.parent/'three_arm_call.go'),
               three_arm_sha256=h.sha(__file__), deployment_arms=ARMS)
    for ref, name in [('a', 'source-a'), ('b', 'source-b')]:
        h.checked(h.command(cfg, [str(binary), 'push', '--store', cfg['store'], '--ref', ref,
                                  '--json', '--quiet', str(root/name)], root, 'push-'+ref))
        cfg['base_manifest' if ref == 'a' else 'target_manifest'] = json.loads((root/('push-'+ref+'.stdout')).read_bytes())['manifest']
    h.checked(h.command(cfg, [str(binary), 'pull', '--store', cfg['store'], '--cache', cfg['cache_dir'],
                              '--json', cfg['target_manifest'], str(root/'seed-output')], root, 'seed-target-cache'))
    require(h.inventory(root/'seed-output') == h.inventory(root/'source-b'), 'Fixture target seed mismatch')
    h.save(root/'config.json', cfg)
    cfg = h.freeze(root/'config.json', root/'frozen.json')
    check_extra_sources(cfg)
    result = run_matrix(root/'frozen.json', root/'smoke', 'smoke', traced=True)
    require(result['valid'], 'Actual synthetic three-arm fixture failed; preserve attempt')
    check_smoke(h.sha(root/'frozen.json'), True, result, cfg)
    result.update(fixture=True, evidence_scope='Synthetic CLI/driver/trace correctness only; not model performance')
    h.save(root/'fixture-result.json', result)
    return result


def run_matrix(frozen_path, output, stage, traced=False, smoke_path=None):
    require(sys.platform == 'linux', 'Actual cohorts require Linux')
    cfg = json.loads(Path(frozen_path).read_bytes())
    frozen_sha = h.sha(frozen_path)
    require(stage in ['smoke', 'main'], 'Unknown stage')
    if stage == 'main':
        require(smoke_path is not None, 'Missing excluded smoke')
        check_smoke(frozen_sha, traced, json.loads(Path(smoke_path).read_bytes()), cfg)
    h.ensure_disjoint(output, cfg)
    h.check_frozen(cfg)
    check_extra_sources(cfg)
    require(cfg['deployment_arms'] == ARMS and cfg['cache_inventory'], 'Three policies and complete cache required')
    output = Path(output).resolve()
    output.mkdir(exist_ok=False)
    for src, name in [(frozen_path, 'frozen.json'), (__file__, 'three_arm.py'), (h.__file__, 'causal.py')]:
        shutil.copyfile(src, output/name)
    n = 1 if stage == 'smoke' else 5
    cohort = dict(status='running', valid=False, stage=stage, traced=traced, arms=ARMS,
                  order=orders(n), trials=[], frozen_sha256=frozen_sha, host=h.host_info(),
                  physical_io=None, byte_scope='Successful command syscall bytes only; setup and common later validation excluded',
                  donor_validation_scope='After common target-ready endpoint, separately timed')
    h.save(output/'cohort.json', cohort)
    try:
        for block, order in enumerate(cohort['order'], 1):
            for arm in order:
                directory = output/('block-%02d-%s' % (block, arm))
                row = trial(cfg, arm, directory, traced)
                row['run_id'] = directory.name
                cohort['trials'].append(row)
                h.save(output/'cohort.json', cohort)
                require(row['status'] == 'succeeded' and row['valid'], 'Failed/invalid attempt retained; no replacement')
                h.cleanup_trial(directory, cfg.get('retain_outputs', False))
        h.check_frozen(cfg)
        check_extra_sources(cfg)
        cohort['summary'] = h.summarize(cohort['trials'], ARMS, n)
        require(all(s['valid'] for s in cohort['summary'].values()), 'Incomplete three-arm cohort')
        cohort.update(status='succeeded', valid=True)
    except Exception as error:
        cohort.update(status='failed', valid=False, error=repr(error),
                      summary=h.summarize(cohort['trials'], ARMS, n, cohort_valid=False))
    h.save(output/'cohort.json', cohort)
    return cohort


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    p = sub.add_parser('prepare')
    p.add_argument('--legacy', type=Path, required=True)
    p.add_argument('--driver', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    p = sub.add_parser('fixture')
    p.add_argument('--onggi', type=Path, required=True)
    p.add_argument('--driver', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    p = sub.add_parser('run')
    p.add_argument('--frozen', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    p.add_argument('--stage', choices=['smoke', 'main'], required=True)
    p.add_argument('--trace', action='store_true')
    p.add_argument('--smoke', type=Path)
    args = parser.parse_args()
    if args.action == 'prepare':
        result = prepare(args.legacy, args.driver, args.output)
    elif args.action == 'fixture':
        result = fixture(args.onggi, args.driver, args.output)
    else:
        result = run_matrix(args.frozen, args.output, args.stage, args.trace, args.smoke)
    print(json.dumps(dict(status=result.get('status'), valid=result.get('valid'), trials=len(result.get('trials', [])))))
    if args.action != 'prepare' and not result['valid']:
        sys.exit(1)
