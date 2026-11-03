"""R/L common readiness cohort; experimental CPU file execution only."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import random
import re
import shutil
import statistics
import subprocess
import sys
import time
import types

import diff_keep
import literal_patch as patch

HELPER = Path(__file__).parents[1]/'new-causal/causal.py'
HELPER_SHA = '1546801671fc19d3871487b2d2d4f0f8e445bae82cbfd25aabf9e87351554f5a'
MODEL_PAIR = dict(
    base=[dict(path='decode_S256_B2_lm.rbln', bytes=15238278850,
               sha256='8919e9fea5c9fddc6f5941dbd80364790eeff1cc55b13c153c1a14991ee59332')],
    target=[dict(path='decode_S256_B8_lm.rbln', bytes=15228048882,
                 sha256='304ff369a72c21afbbc84b7b09639503cd0e6327320a3ec23664d75e6c13460d')])
helper_raw = HELPER.read_bytes()
if hashlib.sha256(helper_raw).hexdigest() != HELPER_SHA:
    raise ValueError('historical helper changed')
h = types.ModuleType('checked_range_cohort_helper')
h.__file__ = str(HELPER)
exec(compile(helper_raw, str(HELPER), 'exec'), h.__dict__)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def save(path, value):
    path = Path(path)
    temporary = path.with_suffix(path.suffix+'.tmp')
    temporary.write_text(json.dumps(value, indent=2)+'\n', encoding='utf-8')
    temporary.replace(path)


def sources():
    return {p.name: h.sha(p) for p in [Path(__file__), Path(patch.__file__), Path(diff_keep.__file__), HELPER]}


def sync_tree(root):
    for path in Path(root).rglob('*'):
        if path.is_file():
            # Windows CRT commit requires write access; Linux keeps the existing read-only sync contract.
            with path.open('rb' if os.name == 'posix' else 'r+b') as stream:
                os.fsync(stream.fileno())


def guard(output, cfg):
    output = Path(output).resolve()
    paths = [cfg[k] for k in ['base_dir', 'target_dir', 'store', 'cache_dir', 'patch_dir', 'driver']]
    paths += [r['path'] for r in cfg['build_receipts']]
    for value in paths:
        source = Path(value).resolve()
        require(output != source and output not in source.parents and source not in output.parents,
                'output overlaps an immutable source')


def check_frozen(cfg):
    require(cfg.get('dataset_kind') in ['fixture','model'], 'dataset scope missing')
    if cfg['dataset_kind'] == 'model':
        require(sys.platform == 'linux' and all(cfg[k+'_inventory'] == MODEL_PAIR[k] for k in MODEL_PAIR),
                'model cohort requires the preregistered complete directed pair on Linux')
    require(cfg.get('source_pins') == sources(), 'measured source changed')
    require(h.sha(cfg['driver']) == cfg['driver_sha256'], 'driver changed')
    require(cfg.get('gomemlimit') == os.environ.get('GOMEMLIMIT'), 'environment changed')
    for receipt in cfg['build_receipts']:
        require(h.sha(receipt['path']) == receipt['sha256'], 'external build receipt changed')
    for key in ['base', 'target', 'cache', 'store', 'patch']:
        path = cfg['store'] if key == 'store' else cfg[key+'_dir']
        require(h.inventory(path) == cfg[key+'_inventory'], 'immutable inventory changed: '+key)
    require(h.sha(Path(cfg['patch_dir'])/'preparation.json') == cfg['patch_preparation_sha256'], 'patch preparation changed')
    doc, _ = patch.checked_patch(cfg['patch_dir'], cfg['patch_manifest_sha256'])
    base, target = cfg['base_inventory'][0], cfg['target_inventory'][0]
    require((doc['base_sha256'], doc['base_bytes'], doc['target_sha256'], doc['target_bytes']) ==
            (base['sha256'], base['bytes'], target['sha256'], target['bytes']), 'patch/input relation differs')
    return doc


def freeze(config, output):
    cfg = json.loads(Path(config).read_bytes(), object_pairs_hook=patch.unique_object)
    require(cfg.get('status') == 'not_run' and cfg.get('dataset_kind') in ['fixture','model']
            and cfg.get('workers') == 8 and cfg.get('gomaxprocs') == 32, 'fresh scope/parallelism required')
    require(type(cfg.get('cleanup_success_data')) is bool and type(cfg.get('command_timeout_seconds')) is int
            and 1 <= cfg['command_timeout_seconds'] <= 600 and type(cfg.get('free_margin_bytes')) is int
            and cfg['free_margin_bytes'] > 0, 'explicit cleanup/deadline/free-space choices required')
    require(re.fullmatch('[0-9a-f]{40}', cfg.get('source_revision','')) and cfg.get('limits_description')
            and cfg.get('build_receipts'), 'source/build/resource provenance required')
    for key in ['base_manifest','target_manifest']:
        require(re.fullmatch('sha256:[0-9a-f]{64}', cfg.get(key,'')), 'explicit manifest root required')
    for key in ['base','target']:
        require(isinstance(cfg.get(key+'_inventory'), list) and len(cfg[key+'_inventory']) == 1,
                'exactly one predeclared file per artifact is required')
        row = cfg[key+'_inventory'][0]
        diff_keep.artifact_path(row['path'])
        require(type(row['bytes']) is int and row['bytes'] >= 0 and re.fullmatch('[0-9a-f]{64}', row['sha256']),
                'complete input identity required')
    for key in ['base_dir','target_dir','store','cache_dir','patch_dir','driver']:
        require(not Path(cfg[key]).is_symlink(), 'symlinked source root')
        cfg[key] = str(Path(cfg[key]).resolve(strict=True))
    for receipt in cfg['build_receipts']:
        receipt['path'] = str(Path(receipt['path']).resolve(strict=True))
    guard(output, cfg)
    require(not Path(output).exists(), 'frozen output already exists')
    for key in ['cache','store','patch']:
        cfg[key+'_inventory'] = h.inventory(cfg['store'] if key == 'store' else cfg[key+'_dir'])
    cfg.update(source_pins=sources(), gomemlimit=os.environ.get('GOMEMLIMIT'),
               host=h.host_info(), frozen_utc=datetime.now(timezone.utc).isoformat())
    doc = check_frozen(cfg)
    cfg['expected_kept_bytes'] = sum(r[1] for r in doc['keep_ranges'])
    save(output, cfg)
    return cfg


def orders(n):
    require(type(n) is int and n in [1,5], 'only excluded smoke1/main5 blocks are defined')
    return [['R','L'] if i % 2 == 0 else ['L','R'] for i in range(n)]


def validate_method(cfg, arm, report):
    if arm == 'R':
        stats = report.get('stats',{})
        require(report.get('admitted') is True and report.get('claim_calls') == 1 and report.get('policy') == 'R'
                and report.get('manifest') == cfg['target_manifest'] and stats.get('base') == cfg['base_manifest']
                and all(type(stats.get(k)) is int and stats[k] == 0 for k in ['read_bytes','peer_bytes','cached','local_bytes'])
                and stats.get('reused') == cfg['expected_kept_bytes'] > 0, 'wrong admitted/cache/retention treatment')
    else:
        result = report.get('result',{})
        literal_bytes = cfg['target_inventory'][0]['bytes']-cfg['expected_kept_bytes']
        require(report.get('status') == 'completed' and report.get('operation') == 'apply'
                and report.get('source_sha256') == cfg['source_pins']['literal_patch.py']
                and result.get('target_sha256') == cfg['target_inventory'][0]['sha256']
                and result.get('manifest_sha256') == cfg['patch_manifest_sha256']
                and result.get('kept_bytes') == cfg['expected_kept_bytes']
                and result.get('base_read_bytes') == cfg['base_inventory'][0]['bytes']
                and result.get('target_validation_read_bytes') == cfg['target_inventory'][0]['bytes']
                and result.get('literal_written_bytes') == result.get('literal_read_bytes') == literal_bytes
                and result.get('cleanup_error') is None, 'patch verification/cleanup did not complete')


def execute(cfg, argv, directory):
    environment = dict(os.environ, GOMAXPROCS='32', LC_ALL='C', TZ='UTC', TMPDIR=str(directory/'tmp'))
    actual = (['nice','-n','19'] if os.name == 'posix' else [])+list(map(str,argv))
    row = dict(argv=actual, environment={k:environment.get(k) for k in ['GOMAXPROCS','GOMEMLIMIT','LC_ALL','TZ','TMPDIR']},
               start_ns=time.perf_counter_ns(), utc_start=datetime.now(timezone.utc).isoformat(), status='running')
    try:
        with (directory/'deploy.stdout').open('xb') as out, (directory/'deploy.stderr').open('xb') as err:
            completed = subprocess.run(actual, stdout=out, stderr=err, env=environment,
                                       timeout=cfg['command_timeout_seconds'])
        row.update(exit_code=completed.returncode, status='succeeded' if completed.returncode == 0 else 'failed')
    except (OSError, subprocess.TimeoutExpired) as error:
        row.update(exit_code=None, status='failed', error=str(error))
    row['end_ns'] = time.perf_counter_ns()
    save(directory/'command.json', row)
    return row


def trial(cfg, arm, directory):
    require(arm in ['R','L'], 'unknown treatment')
    guard(directory, cfg)
    directory = Path(directory).resolve()
    directory.mkdir()
    row = dict(arm=arm, status='running', valid=False, dataset_kind=cfg['dataset_kind'])
    save(directory/'result.json', row)
    prep_start = time.perf_counter_ns()
    try:
        needed = sum(r['bytes'] for r in cfg['base_inventory'])
        needed += sum(r['bytes'] for r in cfg['cache_inventory' if arm == 'R' else 'patch_inventory'])
        require(shutil.disk_usage(directory).free >= needed+cfg['free_margin_bytes'], 'insufficient space; no partial trial')
        row['base_copy'] = h.copy_base(cfg['base_dir'], directory/'base')
        sync_tree(directory/'base')
        row['seed_copy'] = h.copy_base(cfg['cache_dir' if arm == 'R' else 'patch_dir'], directory/'seed')
        sync_tree(directory/'seed')
        for name in ['stage','ready','tmp']:
            (directory/name).mkdir()
        before = (directory/'base'/cfg['base_inventory'][0]['path']).stat()
        row['base_inode_before'] = [before.st_dev, before.st_ino]
        row['preparation_seconds'] = (time.perf_counter_ns()-prep_start)/1e9
        base_file = directory/'base'/cfg['base_inventory'][0]['path']
        target_file = directory/'stage/artifact'/cfg['target_inventory'][0]['path']
        row['online_start_ns'] = time.perf_counter_ns()
        if arm == 'R':
            base_hash, base_reads = patch.sha_file(base_file)
            require(base_hash == cfg['base_inventory'][0]['sha256'], 'base identity differs before consumption')
            row['base_verification_read_bytes'] = base_reads
            argv = [cfg['driver'],'--store',cfg['store'],'--cache',str(directory/'seed'),
                    '--base',cfg['base_manifest'],'--base-dir',str(directory/'base'),
                    '--policy','R','--workers','8',cfg['target_manifest'],str(directory/'stage/artifact')]
        else:
            target_file.parent.mkdir(parents=True)
            argv = [sys.executable,'-B',str(Path(patch.__file__)), 'apply','--base',str(base_file),
                    '--patch',str(directory/'seed'),'--manifest-sha256',cfg['patch_manifest_sha256'],
                    '--exclusive','--output',str(target_file)]
        row['command'] = execute(cfg, argv, directory)
        require(row['command']['status'] == 'succeeded', 'method command failed; retained logs/work')
        report = row['method'] = json.loads((directory/'deploy.stdout').read_bytes())
        expected = cfg['target_inventory'][0]
        validate_method(cfg,arm,report)
        if arm == 'R':
            row['sync_start_ns'] = time.perf_counter_ns()
            sync_tree(directory/'stage/artifact')
            row['sync_finish_ns'] = time.perf_counter_ns()
            require(h.inventory(directory/'stage/artifact') == cfg['target_inventory'], 'private target SHA/inventory differs')
            row['target_verified_ns'] = time.perf_counter_ns()
        else:
            result = report.get('result',{})
            row['base_verification_read_bytes'] = result['base_read_bytes']
            row['target_verified_ns'] = time.perf_counter_ns()
        info = target_file.stat()
        require(not base_file.exists() and [info.st_dev,info.st_ino] == row['base_inode_before'], 'donor not consumed in place')
        row['target_inode_before_publication'] = [info.st_dev, info.st_ino]
        published = directory/'ready'/expected['path']
        published.parent.mkdir(parents=True, exist_ok=True)
        os.link(target_file, published)
        row['published_ns'] = time.perf_counter_ns()
        row['ready_seconds'] = (row['published_ns']-row['online_start_ns'])/1e9
        row['preparation_inclusive_seconds'] = (row['published_ns']-prep_start)/1e9
        row['audit_start_ns'] = time.perf_counter_ns()
        require(h.inventory(directory/'ready') == cfg['target_inventory'], 'independent ready audit differs')
        row['audit_finish_ns'] = time.perf_counter_ns()
        row.update(target_sha256=expected['sha256'], status='succeeded', valid=True)
        row['retained_data_bytes'] = sum(p.stat().st_size for name in ['base','seed','stage','ready','tmp']
                                         for p in (directory/name).rglob('*') if p.is_file())
        row['retained_data_scope'] = 'logical path sizes; stage/ready hard-link alias counted twice, not physical occupancy'
        save(directory/'result.json', row)
        if cfg['cleanup_success_data']:
            start = time.perf_counter_ns()
            paths = [directory/name for name in ['base','seed','stage','ready','tmp']]
            require(all(not p.is_symlink() and p.resolve().parent == directory for p in paths), 'cleanup path escaped owned trial')
            for path in paths:
                shutil.rmtree(path)
            row['successful_data_cleanup_seconds'] = (time.perf_counter_ns()-start)/1e9
    except Exception as error:
        row.update(status='failed', valid=False, error=repr(error))
    save(directory/'result.json', row)
    return row


def summarize(rows):
    summary = {}
    for arm in ['R','L']:
        values = [r['ready_seconds'] for r in rows if r['arm'] == arm]
        q = statistics.quantiles(values, n=4, method='inclusive') if len(values)>1 else [values[0]]*3
        summary[arm] = dict(n=len(values), median=statistics.median(values), iqr=q[2]-q[0], samples=values)
    return summary


def smoke_admission(report, frozen_sha, cfg):
    require(report.get('stage') == 'smoke' and report.get('frozen_sha256') == frozen_sha
            and report.get('dataset_kind') == cfg['dataset_kind'] and report.get('status') == 'succeeded'
            and report.get('valid') is True and report.get('order') == orders(1), 'wrong/incomplete smoke')
    rows = report.get('trials',[])
    require(len(rows) == 2 and [r.get('arm') for r in rows] == ['R','L'], 'missing smoke attempts')
    for row in rows:
        validate_method(cfg,row['arm'],row.get('method',{}))
        require(row.get('status') == 'succeeded' and row.get('valid') is True
                and row.get('dataset_kind') == cfg['dataset_kind']
                and row.get('target_sha256') == cfg['target_inventory'][0]['sha256']
                and row.get('command',{}).get('status') == 'succeeded'
                and row.get('command',{}).get('exit_code') == 0
                and row.get('published_ns',0) >= row.get('target_verified_ns',1)
                and row.get('audit_finish_ns',0) >= row.get('audit_start_ns',1) >= row['published_ns']
                and row.get('target_inode_before_publication') == row.get('base_inode_before'), 'invalid smoke witness')
    require(report.get('summary') == summarize(rows), 'smoke aggregate differs')


def run(frozen, output, stage, *, smoke_path=None, smoke_sha256=None):
    require(stage in ['smoke','main'], 'unknown stage')
    cfg = json.loads(Path(frozen).read_bytes())
    frozen_sha = h.sha(frozen)
    guard(output,cfg)
    if stage == 'main':
        require(smoke_path is not None and smoke_sha256 is not None, 'main requires independently pinned smoke')
        require(h.sha(smoke_path) == smoke_sha256, 'smoke identity changed')
        smoke_admission(json.loads(Path(smoke_path).read_bytes()), frozen_sha, cfg)
    check_frozen(cfg)
    output = Path(output).resolve()
    output.mkdir()
    report = dict(stage=stage, dataset_kind=cfg['dataset_kind'], status='running', valid=False,
                  frozen_sha256=frozen_sha, source_pins=cfg['source_pins'], order=orders(1 if stage=='smoke' else 5),
                  trials=[], summary={}, smoke_sha256=smoke_sha256, started_utc=datetime.now(timezone.utc).isoformat())
    save(output/'cohort.json',report)
    for block, arms in enumerate(report['order'],1):
        for arm in arms:
            name = 'block-%02d-%s'%(block,arm)
            row = trial(cfg,arm,output/name)
            row['run_id'] = name
            report['trials'].append(row)
            save(output/'cohort.json',report)
            if not row['valid']:
                report.update(status='failed', valid=False, finished_utc=datetime.now(timezone.utc).isoformat())
                save(output/'cohort.json',report)
                return report
    report.update(status='succeeded', valid=True, summary=summarize(report['trials']),
                  finished_utc=datetime.now(timezone.utc).isoformat())
    save(output/'cohort.json',report)
    return report


def fixture(binary, driver, output):
    root = Path(output).resolve()
    root.mkdir()
    raw = random.Random(96).randbytes(4<<20)
    changed = bytearray(raw)
    changed[2<<20:(2<<20)+1024] = b'x'*1024
    for name, data in [('base',raw),('target',changed)]:
        (root/name).mkdir()
        (root/name/(name+'.bin')).write_bytes(data)
    manifests = []
    for ref in ['base','target']:
        row = subprocess.run([str(binary),'push','--store',str(root/'store'),'--ref',ref,'--json','--quiet',str(root/ref)],
                             check=True,capture_output=True)
        (root/(ref+'-push.json')).write_bytes(row.stdout)
        manifests.append(json.loads(row.stdout)['manifest'])
    diff = subprocess.run([str(binary),'diff','--store',str(root/'store'),'--ranges','--json',*manifests],check=True,capture_output=True)
    (root/'diff.json').write_bytes(diff.stdout)
    prepared = diff_keep.prepare(root/'base/base.bin',root/'target/target.bin',root/'diff.json',root/'patch',
        diff_sha256=h.sha(root/'diff.json'),base_path='base.bin',target_path='target.bin',
        base_sha256=h.sha(root/'base/base.bin'),target_sha256=h.sha(root/'target/target.bin'))
    subprocess.run([str(binary),'pull','--store',str(root/'store'),'--cache',str(root/'cache'),
                    '--json',manifests[1],str(root/'seed-target')],check=True,capture_output=True)
    cfg = dict(status='not_run',dataset_kind='fixture',workers=8,gomaxprocs=32,
        base_dir=str(root/'base'),target_dir=str(root/'target'),store=str(root/'store'),cache_dir=str(root/'cache'),
        patch_dir=str(root/'patch'),patch_manifest_sha256=prepared['patch']['manifest_sha256'],
        patch_preparation_sha256=h.sha(root/'patch/preparation.json'),driver=str(Path(driver).resolve()),driver_sha256=h.sha(driver),
        base_manifest=manifests[0],target_manifest=manifests[1],base_inventory=h.inventory(root/'base'),target_inventory=h.inventory(root/'target'),
        source_revision='e1b29c7d89d6ac9abc5594414715bcc927dad60b',
        build_receipts=[dict(path=str(Path(binary).resolve()),sha256=h.sha(binary))],
        limits_description='synthetic real Go CLI fixture; binary pin is not model build attestation',
        cleanup_success_data=True,command_timeout_seconds=30,free_margin_bytes=1<<20)
    save(root/'config.json',cfg)
    freeze(root/'config.json',root/'frozen.json')
    smoke = run(root/'frozen.json',root/'smoke','smoke')
    require(smoke['valid'],'synthetic R/L smoke failed; retain fixture')
    return run(root/'frozen.json',root/'main','main',smoke_path=root/'smoke/cohort.json',smoke_sha256=h.sha(root/'smoke/cohort.json'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='operation',required=True)
    p = sub.add_parser('freeze')
    p.add_argument('--config',required=True)
    p.add_argument('--output',required=True)
    p = sub.add_parser('run')
    p.add_argument('--frozen',required=True)
    p.add_argument('--output',required=True)
    p.add_argument('--stage',choices=['smoke','main'],required=True)
    p.add_argument('--smoke-path')
    p.add_argument('--smoke-sha256')
    args = vars(parser.parse_args())
    operation = args.pop('operation')
    result = freeze(**args) if operation == 'freeze' else run(args.pop('frozen'), args.pop('output'), args.pop('stage'), **args)
    print(json.dumps(dict(status=result.get('status'),valid=result.get('valid'),summary=result.get('summary'))))
    return 0 if operation == 'freeze' or result['valid'] else 1


if __name__ == '__main__':
    sys.exit(main())
