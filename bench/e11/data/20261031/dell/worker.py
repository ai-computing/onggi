"""One CPU-only Job per node; verified files and disposable per-trial inodes."""
import base64
import hashlib
import json
import os
import pathlib
import shutil
import subprocess
import time
import traceback
import urllib.request

from common import file_sha, save
from origin import nic_info

ROOT = pathlib.Path('/work')
ADMIN = os.environ['E11_ADMIN'].rstrip('/')
STORE = os.environ['E11_STORE']
NODE = os.environ['NODE_NAME']
BIN = ROOT/'onggi'
ENV = dict(os.environ, AWS_ACCESS_KEY_ID='onggitest', AWS_SECRET_ACCESS_KEY='onggitestsecret',
           GOMAXPROCS='2', GOMEMLIMIT='2500MiB')


def api(path, data=None):
    request = urllib.request.Request(ADMIN+path, data=None if data is None else json.dumps(data).encode(),
                                     headers={'Content-Type':'application/json'})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def run(args, folder, stem):
    folder.mkdir(parents=True, exist_ok=True)
    begin = time.monotonic()
    try:
        p = subprocess.run([str(BIN)]+args, env=ENV, capture_output=True, timeout=1200)
    except subprocess.TimeoutExpired as exc:
        (folder/(stem+'.stdout')).write_bytes(exc.stdout or b'')
        (folder/(stem+'.stderr')).write_bytes(exc.stderr or b'')
        raise
    elapsed = time.monotonic()-begin
    (folder/(stem+'.stdout')).write_bytes(p.stdout)
    (folder/(stem+'.stderr')).write_bytes(p.stderr)
    return dict(returncode=p.returncode, wall_s=elapsed, argv=args,
                stdout=p.stdout.decode(errors='replace'), stderr=p.stderr.decode(errors='replace'))


def verify(folder, artifact):
    start = time.monotonic()
    actual = []
    for item in artifact['files']:
        p = folder/item['path']
        assert p.is_file() and not p.is_symlink(), str(p)
        actual.append(dict(path=item['path'], bytes=p.stat().st_size, sha256=file_sha(p)))
    expected = sorted(artifact['files'],key=lambda x:x['path'])
    actual.sort(key=lambda x:x['path'])
    assert actual == expected, 'output SHA/size mismatch'
    assert sorted(p.relative_to(folder).as_posix() for p in folder.rglob('*') if p.is_file()) == [p['path'] for p in expected]
    return dict(hashes_match=True, files=actual, hash_validation_s=time.monotonic()-start)


def consumable(folder, inputs):
    start = time.monotonic()
    # cp creates independent inodes even when reflinks are available.
    subprocess.run(['cp','-a','--reflink=auto',str(ROOT/'base'),str(folder)],check=True)
    for item in inputs['artifacts']['base']['files']:
        src, dst = ROOT/'base'/item['path'], folder/item['path']
        assert src.stat().st_ino != dst.stat().st_ino
        assert src.stat().st_size == dst.stat().st_size
    return time.monotonic()-start


def init_cache(folder, inputs):
    for item in inputs['store_config']:
        p = folder/item['path']
        p.parent.mkdir(parents=True,exist_ok=True)
        raw = base64.b64decode(item['base64'])
        assert hashlib.sha256(raw).hexdigest() == item['sha256']
        p.write_bytes(raw)


def trial(command, inputs):
    name = command['trial']
    work = ROOT/'runs'/name
    work.mkdir(parents=True,exist_ok=False)
    result = dict(node=NODE, trial=name, status='failed', hashes_match=False,
                  command=command, utc=time.time(), pod_uid=os.environ['POD_UID'])
    peer = None
    try:
        copy_s = consumable(work/'base',inputs)
        cache = work/'cache'
        if command['stratum'] == 'one-seed' and command['seed']:
            shutil.copytree(ROOT/'seed-cache',cache)
        else:
            init_cache(cache,inputs)
        cache_files = [dict(path=p.relative_to(cache).as_posix(),bytes=p.stat().st_size,sha256=file_sha(p))
                       for p in cache.rglob('*') if p.is_file()]
        if command['arm'] == 'peer':
            peer = subprocess.Popen([str(BIN),'peer','serve','--cache',str(cache),
                '--listen',':7071','--refresh','1h'],env=ENV,
                stdout=open(work/'peer.log','ab'),stderr=subprocess.STDOUT)
            deadline = time.monotonic()+30
            while True:
                try:
                    req=urllib.request.Request('http://127.0.0.1:7071/v1/has',data=b'',method='POST')
                    with urllib.request.urlopen(req,timeout=2) as r:
                        assert r.read() == b''
                    break
                except OSError:
                    assert peer.poll() is None and time.monotonic()<deadline
                    time.sleep(.05)
        ready = dict(node=NODE,trial=name,copy_s=copy_s,cache_before=cache_files,
                     nics=nic_info(),ready_utc=time.time())
        save(work/'ready.json',ready)
        api('/ready',ready)
        while True:
            barrier=api('/barrier/'+name)
            if barrier['failed']:
                raise RuntimeError('central barrier failed')
            if barrier['released']:
                break
            time.sleep(.01)
        api('/started',dict(node=NODE,trial=name))
        args=['pull','--store',STORE,'--workers','2','--json','--cache',str(cache),
              '--base',inputs['artifacts']['base']['manifest'],'--base-dir',str(work/'base')]
        if command['arm'] == 'peer':
            args += ['--peers',','.join(command['peers'])]
        args += [inputs['artifacts']['target']['manifest'],str(work/'out')]
        pulled=run(args,work,'pull')
        # Completion marker precedes full-file SHA work; receipt uses one central monotonic clock.
        api('/pulled',dict(node=NODE,trial=name,returncode=pulled['returncode']))
        result.update(pulled, pull_s=pulled['wall_s'], nics_after=nic_info())
        assert pulled['returncode'] == 0, pulled['stderr']
        report=json.loads(pulled['stdout'])
        assert report['manifest'] == inputs['artifacts']['target']['manifest']
        result.update(report)
        result.update(verify(work/'out',inputs['artifacts']['target']))
        assert result['stats']['base'] == inputs['artifacts']['base']['manifest']
        result['status']='completed'
    except BaseException:
        result['error']=traceback.format_exc()
    finally:
        if peer:
            peer.terminate()
            peer.wait(timeout=20)
        if (work/'peer.log').exists():
            result['peer_log']=(work/'peer.log').read_text()
        if (work/'pull.stdout').exists():
            result.setdefault('stdout',(work/'pull.stdout').read_text(errors='replace'))
            result.setdefault('stderr',(work/'pull.stderr').read_text(errors='replace'))
        save(work/'result.json',result)
        # Delete only owned trial bulk payloads AFTER evidence is written and acknowledged.
        cleanup_start=time.monotonic()
        try:
            for label in ['base','out','cache']:
                target=(work/label).resolve()
                assert target.is_relative_to((ROOT/'runs').resolve()) and target != ROOT/'runs'
                if target.exists():
                    shutil.rmtree(target)
            result['trial_payloads_absent']=True
        except BaseException:
            result['status']='failed'
            result['cleanup_error']=traceback.format_exc()
        result['cleanup_s']=time.monotonic()-cleanup_start
        save(work/'result.json',result)
        api('/result',result)
    if result['status'] != 'completed':
        raise RuntimeError(result.get('error','trial failed'))


def main():
    ROOT.mkdir(exist_ok=True)
    with urllib.request.urlopen(ADMIN+'/inputs.json',timeout=30) as r:
        inputs=json.load(r)
    save(ROOT/'inputs.json',inputs)
    with urllib.request.urlopen(ADMIN+'/assets/onggi',timeout=60) as r, BIN.open('wb') as f:
        shutil.copyfileobj(r,f)
    BIN.chmod(0o755)
    assert file_sha(BIN) == inputs['tools']['onggi']['sha256']
    bootstrap=run(['pull','--store',STORE,'--workers','2','--json',
                   inputs['artifacts']['base']['manifest'],str(ROOT/'base')],ROOT,'bootstrap')
    assert bootstrap['returncode']==0,bootstrap['stderr']
    bootstrap.update(verify(ROOT/'base',inputs['artifacts']['base']))
    seed=None
    if NODE=='cpu-1':
        seed_copy=consumable(ROOT/'seed-base',inputs)
        seed=run(['pull','--store',STORE,'--workers','2','--json','--cache',str(ROOT/'seed-cache'),
                  '--base',inputs['artifacts']['base']['manifest'],'--base-dir',str(ROOT/'seed-base'),
                  inputs['artifacts']['target']['manifest'],str(ROOT/'seed-out')],ROOT,'seed')
        assert seed['returncode']==0,seed['stderr']
        seed.update(verify(ROOT/'seed-out',inputs['artifacts']['target']))
        seed['copy_s']=seed_copy
        shutil.rmtree(ROOT/'seed-out')
        if (ROOT/'seed-base').exists():
            shutil.rmtree(ROOT/'seed-base')
    registration=dict(node=NODE,ip=os.environ['POD_IP'],pod_uid=os.environ['POD_UID'],
        bootstrap=bootstrap,seed=seed,nics=nic_info(),python=os.sys.version,
        device_files=[p.name for p in pathlib.Path('/dev').iterdir() if any(x in p.name.lower() for x in ['rbln','furiosa','rngd','nvidia'])],
        binary_sha256=file_sha(BIN),seed_cache_inventory=[dict(path=p.relative_to(ROOT/'seed-cache').as_posix(),
            bytes=p.stat().st_size,sha256=file_sha(p)) for p in (ROOT/'seed-cache').rglob('*') if p.is_file()])
    assert not registration['device_files']
    save(ROOT/'registration.json',registration)
    api('/register',registration)
    last=None
    while True:
        command=api('/command/'+NODE)
        if command and command.get('stop'):
            return
        if command and command['trial']!=last:
            trial(command,inputs)
            last=command['trial']
        time.sleep(.2)


if __name__=='__main__':
    main()
