"""Full-set encoding controls using a labeled experimental reference binary."""
import argparse
import base64
import datetime
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import statistics
import struct
import sys
import time
import types

HELPER_SHA='1546801671fc19d3871487b2d2d4f0f8e445bae82cbfd25aabf9e87351554f5a'
_helper=Path(__file__).with_name('causal.py')
_bytes=_helper.read_bytes()
if hashlib.sha256(_bytes).hexdigest()!=HELPER_SHA:raise ValueError('Unexpected historical helper')
h=types.ModuleType('matched_frames_checked_helper');h.__file__=str(_helper)
exec(compile(_bytes,str(_helper),'exec'),h.__dict__)
_reference=Path(__file__).with_name('flat_frame_reference.py')
reference=types.ModuleType('checked_reference_builder');reference.__file__=str(_reference)
exec(compile(_reference.read_bytes(),str(_reference),'exec'),reference.__dict__)
ARMS=['D','I','F','H']
SUB_KINDS=['atom-prefill','atom-decode','atom-other']
FINE=dict(Min=1024,Avg=4096,Max=16384)
COARSE=dict(Min=16384,Avg=65536,Max=262144)
IDENTITY=dict(Min=65536,Avg=262144,Max=1048576)


def require(condition,message):
    if not condition:raise ValueError(message)


def orders(n):
    require(type(n) is int and n in (1,5),'Only smoke1/main5 blocks are defined')
    return [ARMS[i%4:]+ARMS[:i%4] for i in range(n)]


def push_argv(cfg,arm,trial):
    require(arm in ARMS,'Unknown policy')
    fine=arm!='H'
    return [cfg['onggi'],'push','--store',str(trial/'store'),
        '--min','1KiB' if fine else '16KiB','--avg','4KiB' if fine else '64KiB',
        '--max','16KiB' if fine else '256KiB',
        '--sub',dict(D='0',I='256KiB',F='256KiB',H='4KiB')[arm],
        '--sub-kinds',','.join(SUB_KINDS),'--frame','256KiB' if arm in ['F','H'] else '0',
        '--compress','zstd','--pack-size','64MiB','--workers','8','--uploads','4',
        '--quiet','--json','--tmp',str(trial/'tmp'),'--ref','family',cfg['source_dir']]


def check_config(cfg,arm):
    require(arm in ARMS,'Unknown stored policy')
    require(cfg.get('schema')=='onggi.store.v1' and cfg.get('pack_size')==64<<20,'Wrong schema/pack limit')
    require(cfg.get('chunker')==(COARSE if arm=='H' else FINE),'Wrong top-level chunker')
    require(cfg.get('frame',0)==(256<<10 if arm in ['F','H'] else 0),'Wrong effective frame setting')
    if arm=='D':require(not cfg.get('sub_chunker') and not cfg.get('sub_kinds'),'Direct flat policy has sub-chunks')
    else:
        require(cfg.get('sub_chunker')==(FINE if arm=='H' else IDENTITY),'Wrong identity/real sub-split')
        require(cfg.get('sub_kinds')==SUB_KINDS,'Wrong framed file scope')
    return True


def check_dataset(cfg):
    require(cfg.get('dataset_kind') in ['synthetic','model'],'Declare dataset scope')
    rows=cfg.get('input_inventory',[])
    require(bool(rows),'Predeclare the full input inventory')
    names=[]
    for row in rows:
        p=PurePosixPath(row['path'])
        require(p.parts and not p.is_absolute() and '..' not in p.parts and '\\' not in row['path'],
            'Invalid input path')
        require(type(row.get('bytes')) is int and row['bytes']>=0 and
            re.fullmatch('[0-9a-f]{64}',row.get('sha256','')),'Missing size/digest')
        names.append(row['path'])
    require(names==sorted(names) and len(set(names))==len(names),'Input inventory must be sorted and unique')
    if cfg['dataset_kind']=='model':
        require(len(rows)==19 and sum(r['bytes'] for r in rows)==263893021566,
            'Full model family requires exact19 files/263893021566B; subsets are not full-set evidence')
        require(sum(r['path'].lower().endswith('.rbln') for r in rows)==18,'Expected18 opaque compiled files')
    return True


def guard_output(output,cfg):
    output=Path(output).resolve()
    for key in ['source_dir','onggi']:
        source=Path(cfg[key]).resolve()
        require(output!=source and output not in source.parents and source not in output.parents,
            'Output overlaps immutable '+key)


def freeze(config,output):
    cfg=json.loads(Path(config).read_bytes())
    require(cfg.get('status')=='not_run' and cfg.get('workers')==8 and cfg.get('gomaxprocs')==32,
        'Fresh config must fix workers8/GOMAXPROCS32')
    require(re.fullmatch('[0-9a-f]{40}',cfg.get('core_revision','')) and cfg.get('binary_build_provenance')
        and cfg.get('limits_description'),'Missing core/build/resource provenance')
    require(type(cfg.get('free_margin_bytes')) is int and cfg['free_margin_bytes']>0,'Declare free-space margin')
    check_dataset(cfg)
    if cfg['dataset_kind']=='model':require(cfg.get('reference_manifest_path'),'Model policy requires its experimental reference build receipt')
    if cfg.get('reference_manifest_path'):
        proof=Path(cfg['reference_manifest_path']).resolve(strict=True)
        reference.validate_proof(proof,cfg['binary_sha256'])
        cfg.update(reference_manifest_path=str(proof),reference_manifest_sha256=h.sha(proof))
    for key in ['source_dir','onggi']:cfg[key]=str(Path(cfg[key]).resolve(strict=True))
    guard_output(output,cfg)
    require(h.sha(cfg['onggi'])==cfg['binary_sha256'],'Binary digest differs')
    require(h.inventory(cfg['source_dir'])==cfg['input_inventory'],'Predeclared input inventory differs')
    cfg.update(harness_sha256=h.sha(__file__),helper_sha256=HELPER_SHA,config_source_sha256=h.sha(config),
        reference_generator_sha256=h.sha(_reference),
        config_source_path=str(Path(config).resolve()),
        gomemlimit=os.environ.get('GOMEMLIMIT'),policies=ARMS,host=h.host_info(),
        frozen_utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),
        interpretation='F uses identity-sub framing adapter; D/I exposes extra work. Not an optimized native flat-framing implementation.')
    require(not Path(output).exists(),'Frozen output already exists')
    h.save(output,cfg)
    return cfg


def check_frozen(cfg):
    check_dataset(cfg)
    require(cfg.get('harness_sha256')==h.sha(__file__) and cfg.get('helper_sha256')==h.sha(_helper),
        'Measured source changed')
    require(cfg.get('reference_generator_sha256')==h.sha(_reference),'Reference generator source changed')
    require(cfg.get('policies')==ARMS and cfg.get('gomemlimit')==os.environ.get('GOMEMLIMIT'),'Policy/environment changed')
    require(h.sha(cfg['onggi'])==cfg['binary_sha256'],'Binary changed')
    require(h.sha(cfg['config_source_path'])==cfg['config_source_sha256'],'Predeclared configuration changed')
    if cfg.get('reference_manifest_path'):
        require(h.sha(cfg['reference_manifest_path'])==cfg['reference_manifest_sha256'],'Reference build receipt changed')
        reference.validate_proof(cfg['reference_manifest_path'],cfg['binary_sha256'])
    require(h.inventory(cfg['source_dir'])==cfg['input_inventory'],'Immutable input changed')
    return True


def index_witness(root):
    """Stream Onggi's public index format; never inspect vendor model formats."""
    result=dict(stored_entries=0,framed_entries=0,composites=0,objects=[],
        scope='worker-counted index structure; raw header/first-frame witnesses retained, bulk indexes not exported')
    for p in sorted((Path(root)/'index').rglob('*')):
        if not p.is_file():continue
        require(not p.is_symlink(),'Index symlink')
        size=p.stat().st_size;row=dict(path=p.relative_to(root).as_posix(),bytes=size,sha256=h.sha(p))
        with p.open('rb') as stream:
            magic=stream.read(8)
            if magic==b'ONGGICX1':
                raw=stream.read(4);require(len(raw)==4,'Truncated composites')
                count=struct.unpack('<I',raw)[0]
                for _ in range(count):
                    raw=stream.read(36);require(len(raw)==36,'Truncated composite entry')
                    n=struct.unpack('<I',raw[32:])[0]
                    require(n>1 and stream.tell()+36*n<=size,'Invalid composite members')
                    stream.seek(36*n,1)
                require(stream.tell()==size,'Composite trailing bytes')
                row.update(kind='composites',count=count);result['composites']+=count
            else:
                require(magic in [b'ONGGIIX1',b'ONGGIIX2',b'ONGGIIX3'],'Unknown index format')
                rest=stream.read(36);require(len(rest)==36,'Truncated index header')
                n=struct.unpack('<I',rest[32:])[0];width={b'ONGGIIX1':40,b'ONGGIIX2':48,b'ONGGIIX3':56}[magic]
                require(size==44+n*width,'Index count/size mismatch')
                framed=0
                for _ in range(n):
                    raw=stream.read(width)
                    require(len(raw)==width,'Truncated index entry')
                    if width==56:
                        inner,frame=struct.unpack('<II',raw[48:56]);length=struct.unpack('<I',raw[40:44])[0]
                        require(not frame or inner+length<=frame,'Chunk outside frame')
                        if frame:
                            framed+=1
                            if 'first_frame' not in row:row['first_frame']=dict(header=base64.b64encode(magic+rest).decode(),
                                entry=base64.b64encode(raw).decode())
                row.update(kind='stored',count=n,framed=framed)
                result['stored_entries']+=n;result['framed_entries']+=framed
        result['objects'].append(row)
    return result


def manifest_witness(root,digest):
    require(re.fullmatch('sha256:[0-9a-f]{64}',digest),'Bad published manifest')
    d=digest.split(':')[1];p=root/'manifests'/d[:2]/d
    require(h.sha(p)==d,'Manifest digest mismatch')
    raw=p.read_bytes();manifest=json.loads(raw)
    return manifest,dict(path=p.relative_to(root).as_posix(),sha256=d,base64=base64.b64encode(raw).decode())


def trial(cfg,arm,root,flat_lists=None):
    root.mkdir();(root/'tmp').mkdir()
    row=dict(arm=arm,status='running',valid=False,run_id=root.name,trial_root=str(root))
    h.save(root/'result.json',row)
    try:
        row['preparation_start_ns']=time.perf_counter_ns();check_frozen(cfg)
        need=sum(r['bytes'] for r in cfg['input_inventory'])+cfg['free_margin_bytes']
        require(shutil.disk_usage(root).free>=need,'Insufficient store budget before push')
        row['preparation_end_ns']=time.perf_counter_ns()
        row['push']=h.checked(h.command(cfg,push_argv(cfg,arm,root),root,'push'))
        cli=json.loads((root/'push.stdout').read_bytes());row['cli']=cli
        stats=cli['stats']
        require(stats['files']==len(cfg['input_inventory']) and stats['bytes']==sum(r['bytes'] for r in cfg['input_inventory'])
            and not stats.get('skipped'),'CLI did not publish the complete family')
        row['totals']=h.store_totals(root/'store')
        configs=[p for p in (root/'store/config').rglob('*') if p.is_file()]
        require(len(configs)==1,'Expected one actual stored config')
        row['stored_config']=json.loads(configs[0].read_bytes());row['config_valid']=check_config(row['stored_config'],arm)
        row['index']=index_witness(root/'store')
        require(arm=='H' or row['index']['composites']==0,'Logical flat policy made composite chunks')
        require(arm not in ['D','I'] or row['index']['framed_entries']==0,'Frame-off arm made frames')
        manifest,row['manifest_witness']=manifest_witness(root/'store',cli['manifest'])
        expected={r['path']:r['bytes'] for r in cfg['input_inventory']}
        require({f['path']:f['size'] for f in manifest['files']}==expected,'Manifest input coverage differs')
        row['file_lists']={f['path']:f['list'] for f in manifest['files']}
        row['flat_lists_equal']=arm=='H' or flat_lists is None or row['file_lists']==flat_lists
        require(row['flat_lists_equal'],'Identity adapter changed fine chunk lists')
        require(shutil.disk_usage(root).free>=need,'Insufficient complete roundtrip budget after actual publication')
        row['pull']=h.checked(h.command(cfg,[cfg['onggi'],'pull','--store',str(root/'store'),'--workers','8',
            '--json',cli['manifest'],str(root/'output')],root,'pull'))
        row['validation_start_ns']=time.perf_counter_ns()
        row['target_inventory']=h.inventory(root/'output')
        row['target_hash_equal']=row['target_inventory']==cfg['input_inventory']
        row['source_inventory_after']=h.inventory(cfg['source_dir'])
        row['source_hash_equal']=row['source_inventory_after']==cfg['input_inventory']
        row['validation_end_ns']=time.perf_counter_ns()
        require(row['target_hash_equal'] and row['source_hash_equal'],'Full output/source SHA mismatch')
        require(row['push']['environment']['GOMEMLIMIT']==cfg['gomemlimit'],'Memory environment changed')
        row.update(status='succeeded',valid=True)
        validate_row(row,cfg,flat_lists)
    except Exception as error:row.update(status='failed',error=repr(error))
    h.save(root/'result.json',row)
    return row


def distribution(values):
    q=statistics.quantiles(values,method='inclusive') if len(values)>1 else [values[0]]*3
    return dict(values=values,n=len(values),median=statistics.median(values),q1=q[0],q3=q[2],iqr=q[2]-q[0])


def summarize(rows,n,cohort_valid=True):
    valid=cohort_valid and len(rows)==4*n and all(r.get('valid') is True and r.get('status')=='succeeded' for r in rows)
    out={}
    for arm in ARMS:
        samples=[r for r in rows if r['arm']==arm]
        ok=valid and len(samples)==n
        item=dict(valid=ok,samples=len(samples))
        if ok:
            for key in ['pack_bytes','metadata_bytes','finalized_bytes']:
                item[key]=distribution([r['totals'][key] for r in samples])
            item['publication_s']=distribution([r['push']['elapsed_s'] for r in samples])
            for key in ['user_s','system_s','max_rss_kib']:
                values=[r['push'].get('resources',{}).get(key) for r in samples]
                item[key]=distribution(values) if all(type(v) in [int,float] and math.isfinite(v) for v in values) else None
        out[arm]=item
    return out


def validate_row(row,cfg,flat_lists):
    require(all(k in row for k in ['stored_config','index','cli','manifest_witness','file_lists',
        'push','pull','target_inventory','source_inventory_after','trial_root']),'Missing actual treatment witness')
    arm=row['arm'];root=Path(row['trial_root'])
    check_config(row['stored_config'],arm)
    index=row['index']
    require(type(index.get('stored_entries')) is int and index['stored_entries']>0 and
        type(index.get('framed_entries')) is int and 0<=index['framed_entries']<=index['stored_entries'],
        'Missing actual index entries')
    require(arm=='H' or index.get('composites')==0,'Flat policy has composites')
    require(arm not in ['D','I'] or index['framed_entries']==0,'Unexpected frame-off entries')
    require(row['target_inventory']==cfg['input_inventory'] and row['source_inventory_after']==cfg['input_inventory'],
        'Actual input/output inventory differs')
    cli=row['cli'];stats=cli.get('stats',{})
    require(stats.get('files')==len(cfg['input_inventory']) and stats.get('bytes')==sum(f['bytes'] for f in cfg['input_inventory'])
        and not stats.get('skipped'),'Wrong actual CLI family')
    witness=row['manifest_witness'];raw=base64.b64decode(witness['base64'],validate=True)
    md='sha256:'+hashlib.sha256(raw).hexdigest();manifest=json.loads(raw)
    require(cli.get('manifest')==md and witness.get('sha256')==md[7:],'Manifest witness differs')
    require(manifest.get('schema')=='onggi.manifest.v1' and len(manifest.get('files',[]))==len(cfg['input_inventory'])
        and {f['path']:f['size'] for f in manifest['files']}=={f['path']:f['bytes'] for f in cfg['input_inventory']},
        'Manifest witness omits inputs')
    require(row['file_lists']=={f['path']:f['list'] for f in manifest['files']},'File list witness differs')
    require(arm=='H' or flat_lists is None or row['file_lists']==flat_lists,'Actual flat lists differ')
    env=dict(GOMAXPROCS='32',GOMEMLIMIT=cfg['gomemlimit'],LC_ALL='C',TZ='UTC',TMPDIR=str(root/'tmp'))
    expected_pull=[cfg['onggi'],'pull','--store',str(root/'store'),'--workers','8','--json',md,str(root/'output')]
    for label,argv in [('push',push_argv(cfg,arm,root)),('pull',expected_pull)]:
        command=row[label]
        require(command.get('status')=='succeeded' and command.get('exit_code')==0 and
            command.get('argv')==['nice','-n','19']+argv and command.get('environment')==env,
            'Actual command/environment differs')
        require(type(command.get('elapsed_s')) in [int,float] and math.isfinite(command['elapsed_s'])
            and command['elapsed_s']>=0,'Missing actual command clock')
    return True


def check_smoke(smoke,frozen_sha,cfg=None):
    require(cfg is not None,'Smoke admission requires frozen inputs')
    require(smoke.get('stage')=='smoke' and smoke.get('status')=='succeeded' and smoke.get('valid') is True
        and smoke.get('arms')==ARMS and smoke.get('frozen_sha256')==frozen_sha and smoke.get('order')==orders(1),
        'Wrong smoke/cohort identity')
    rows=smoke.get('trials',[])
    require(len(rows)==4 and [r.get('arm') for r in rows]==ARMS,'Missing/reordered actual smoke trials')
    for row in rows:
        require(row.get('run_id')=='block-01-'+row['arm'] and row.get('status')=='succeeded' and
            all(row.get(k) is True for k in ['valid','target_hash_equal','source_hash_equal','config_valid','flat_lists_equal']),
            'Missing smoke treatment/output witness')
        validate_row(row,cfg,rows[0].get('file_lists'))
    require(smoke.get('summary')==summarize(rows,1),'Smoke summary differs')
    return True


def run(frozen,output,stage,smoke=None):
    require(sys.platform=='linux','Actual cohort requires Linux')
    require(stage in ['smoke','main'],'Unknown cohort stage')
    cfg=json.loads(Path(frozen).read_bytes());guard_output(output,cfg);check_frozen(cfg)
    frozen_sha=h.sha(frozen)
    if stage=='main':
        require(smoke is not None,'Main requires matching excluded smoke')
        check_smoke(json.loads(Path(smoke).read_bytes()),frozen_sha,cfg)
    output=Path(output).resolve();output.mkdir(parents=True,exist_ok=False)
    for source,name in [(Path(frozen),'frozen.json'),(Path(__file__),'matched_frames.py'),(_helper,'causal.py'),
                        (Path(cfg['config_source_path']),'config.json'),(_reference,'flat_frame_reference.py')]:
        shutil.copyfile(source,output/name)
    if cfg.get('reference_manifest_path'):
        shutil.copyfile(cfg['reference_manifest_path'],output/'reference-manifest.json')
        for i in range(4):
            for suffix in ['stdout','stderr']:
                name='command-%d.%s'%(i,suffix)
                shutil.copyfile(Path(cfg['reference_manifest_path']).parent/name,output/name)
    n=5 if stage=='main' else 1
    cohort=dict(status='running',valid=False,stage=stage,arms=ARMS,order=orders(n),trials=[],
        frozen_sha256=frozen_sha,host=h.host_info(),scientific_acceptance='pending exact raw replay and build/control provenance')
    h.save(output/'cohort.json',cohort)
    flat_lists=None
    try:
        for i,(tool,args) in enumerate([(cfg['onggi'],['version']),('nice',['--version']),('/usr/bin/time',['--version'])]):
            h.checked(h.execute([tool]+args,output,'tool-'+str(i)))
        for block,order in enumerate(cohort['order'],1):
            for arm in order:
                root=output/('block-%02d-%s'%(block,arm))
                row=trial(cfg,arm,root,flat_lists);cohort['trials'].append(row);h.save(output/'cohort.json',cohort)
                require(row['valid'] and row['status']=='succeeded','Failed attempt retained; no replacement')
                if arm=='D':
                    require(flat_lists is None or flat_lists==row['file_lists'],'Direct fine lists changed across attempts')
                    flat_lists=row['file_lists']
                h.cleanup_trial(root,False)
        check_frozen(cfg)
        cohort.update(status='succeeded',summary=summarize(cohort['trials'],n))
        cohort['valid']=all(r['valid'] for r in cohort['summary'].values())
    except Exception as error:cohort.update(status='failed',error=repr(error),summary=summarize(cohort['trials'],n,cohort_valid=False))
    h.save(output/'cohort.json',cohort)
    return cohort


def fixture(binary,root,reference_manifest=None):
    import random
    root=Path(root);source=root/'inputs';source.mkdir()
    base=random.Random(93).randbytes(16<<10);parts=[]
    for i in range(256):
        b=bytearray(base)
        for j in range(0,len(b),512):b[j:j+8]=struct.pack('<Q',i*64+j)
        parts.append(bytes(b))
    (source/'prefill_C16.rbln').write_bytes(b''.join(parts))
    (source/'prefill_C128.rbln').write_bytes(b''.join(parts[::2]+parts[1::2]))
    (source/'config.json').write_text('{"synthetic":true}\n')
    cfg=dict(status='not_run',dataset_kind='synthetic',source_dir=str(source),input_inventory=h.inventory(source),
        onggi=str(binary),binary_sha256=h.sha(binary),core_revision='0'*40,binary_build_provenance='Actual caller-supplied fixture binary; not model provenance',
        limits_description='Actual CI host; no model performance',workers=8,gomaxprocs=32,free_margin_bytes=8<<20)
    if reference_manifest is not None:cfg['reference_manifest_path']=str(reference_manifest)
    if reference_manifest is not None:cfg['core_revision']=json.loads(Path(reference_manifest).read_bytes())['base_revision']
    h.save(root/'config.json',cfg);freeze(root/'config.json',root/'frozen.json')
    return run(root/'frozen.json',root/'smoke','smoke')


def main():
    p=argparse.ArgumentParser(description=__doc__);s=p.add_subparsers(dest='action',required=True)
    f=s.add_parser('freeze');f.add_argument('--config',type=Path,required=True);f.add_argument('--output',type=Path,required=True)
    r=s.add_parser('run');r.add_argument('--frozen',type=Path,required=True);r.add_argument('--output',type=Path,required=True)
    r.add_argument('--stage',choices=['smoke','main'],required=True);r.add_argument('--smoke',type=Path)
    f=s.add_parser('fixture');f.add_argument('--onggi',type=Path,required=True);f.add_argument('--output',type=Path,required=True)
    a=p.parse_args()
    if a.action=='freeze':result=freeze(a.config,a.output)
    elif a.action=='run':result=run(a.frozen,a.output,a.stage,a.smoke)
    else:a.output.mkdir(exist_ok=False);result=fixture(a.onggi.resolve(),a.output)
    print(json.dumps(result));return 0 if a.action=='freeze' or result['valid'] else 1


if __name__=='__main__':sys.exit(main())
