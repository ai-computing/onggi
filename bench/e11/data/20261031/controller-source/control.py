"""E11 controller: all remote execution goes through audited hub Orca transport."""
import base64
import gzip
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, 'C:/Users/USER/orca/scratch/ccgrid-npu-20261030')
import e6_controller as c
c.LOCAL = ROOT
c.REMOTE = '/home/gspark/workspace/codex_e11_20261031'

def rpc(code, stem):
    return c.rpc('dell_0', code, stem)

def stage(path, target, environment='dell_0'):
    raw = pathlib.Path(path).read_bytes()
    encoded = base64.b64encode(gzip.compress(raw)).decode()
    return c.rpc(environment, f'''import pathlib,json,base64,gzip,hashlib
r=pathlib.Path({c.REMOTE!r});p=pathlib.Path({target!r})
assert p.resolve().is_relative_to(r.resolve())
p.parent.mkdir(parents=True,exist_ok=True)
b=gzip.decompress(base64.b64decode({encoded!r}))
if p.exists(): assert p.read_bytes()==b
else: p.write_bytes(b)
print(json.dumps(dict(path=str(p),sha256=hashlib.sha256(b).hexdigest())))
''', 'stage-'+pathlib.Path(target).name)
