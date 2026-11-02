"""Export exact bounded CPU records; payload trees and delta binaries excluded."""
import argparse
import base64
import gzip
import hashlib
import json
from control import ROOT, REMOTE, rpc

parser = argparse.ArgumentParser()
parser.add_argument('kind', choices=['timing', 'io', 'publish'])
parser.add_argument('stage', choices=['smoke', 'main'])
args = parser.parse_args()
label = args.kind + '-' + args.stage
dest = ROOT / 'cpu-exports' / label
assert not dest.exists()
code = f'''import pathlib,json,hashlib,base64,gzip
r=pathlib.Path({REMOTE!r})/'cpu-model-a01';label={label!r};root=r/label
cohort=json.loads((root/'cohort.json').read_text());assert cohort['status'] in ('succeeded','failed')
paths=[p for p in root.iterdir() if p.is_file() and p.suffix in ('.json','.py','.stdout','.stderr','.resources')]
for trial in root.glob('block-*'):
 if trial.is_dir():paths += [p for p in trial.iterdir() if p.is_file() and (p.suffix in ('.json','.py','.stdout','.stderr','.resources') or '.trace.' in p.name)]
paths += [r/'preparation.json',r/{(args.kind+'-config.json')!r},r/{(args.kind+'-frozen.json')!r}]
rows=[]
for p in sorted(set(paths)):
 assert not p.is_symlink();data=p.read_bytes()
 rows.append(dict(path=p.relative_to(r).as_posix(),bytes=len(data),sha256=hashlib.sha256(data).hexdigest(),base64=base64.b64encode(data).decode()))
packed=gzip.compress(json.dumps(dict(cohort=label,files=rows),separators=(',',':')).encode(),mtime=0)
target=pathlib.Path({REMOTE!r})/'evidence'/('cpu-'+label+'-exact.json.gz');assert not target.exists();target.write_bytes(packed)
print(json.dumps(dict(path=str(target),bytes=len(packed),sha256=hashlib.sha256(packed).hexdigest(),files=len(rows))))
'''
meta = rpc('rebellion', code, 'pack-cpu-' + label)
chunks = []
for offset in range(0, meta['bytes'], 64000):
    code = f'''import pathlib,base64,json
print(json.dumps(dict(data=base64.b64encode(pathlib.Path({meta['path']!r}).read_bytes()[{offset}:{offset+64000}]).decode())))
'''
    chunks.append(base64.b64decode(rpc('rebellion', code, 'fetch-cpu-' + label + '-' + str(offset))['data']))
packed = b''.join(chunks)
assert len(packed) == meta['bytes'] and hashlib.sha256(packed).hexdigest() == meta['sha256']
doc = json.loads(gzip.decompress(packed))
assert doc['cohort'] == label and len(doc['files']) == meta['files']
dest.mkdir(parents=True)
(dest / 'exact-evidence.json.gz').write_bytes(packed)
for entry in doc['files']:
    p = (dest / entry['path']).resolve()
    assert p.is_relative_to(dest.resolve()) and not p.exists()
    data = base64.b64decode(entry['base64'])
    assert len(data) == entry['bytes'] and hashlib.sha256(data).hexdigest() == entry['sha256']
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_bytes(data)
manifest = dict(archive=meta, files=[{k: v for k, v in e.items() if k != 'base64'} for e in doc['files']])
(dest / 'export-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')
print(json.dumps(dict(exported=True, label=label, files=len(doc['files']), archive=meta)))
