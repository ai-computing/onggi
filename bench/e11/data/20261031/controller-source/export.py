"""Export only this experiment's small raw files via hub/orca; verify every byte."""
import base64
import gzip
import hashlib
import io
import json
import pathlib
import tarfile

from control import c

LOCAL=pathlib.Path(__file__).resolve().parent
REMOTE='/home/gspark/workspace/codex_e11_20261031'


def fetch(environment, label, selection):
    code=f'''import pathlib,json,hashlib,tarfile
r=pathlib.Path({REMOTE!r});out=r/{('export-'+label+'.tar.gz')!r}
items=[]
for relative in {selection!r}:
 p=r/relative
 if not p.exists():continue
 for f in ([p] if p.is_file() else p.rglob('*')):
  if not f.is_file() or '__pycache__' in f.parts:continue
  if f.name in ['onggi','versitygw'] or f.suffix=='.pyc':continue
  assert f.resolve().is_relative_to(r.resolve())
  assert f.stat().st_size<12000000,(str(f),f.stat().st_size)
  items.append(f)
items=sorted(set(items))
manifest=[dict(path=f.relative_to(r).as_posix(),bytes=f.stat().st_size,sha256=hashlib.sha256(f.read_bytes()).hexdigest()) for f in items]
with tarfile.open(out,'w:gz') as tar:
 for f in items:tar.add(f,arcname=f.relative_to(r).as_posix(),recursive=False)
print(json.dumps(dict(path=str(out),bytes=out.stat().st_size,sha256=hashlib.sha256(out.read_bytes()).hexdigest(),files=manifest)))'''
    meta=c.rpc(environment,code,'pack-'+label)
    dest=LOCAL/'export'/label
    dest.mkdir(parents=True,exist_ok=False)
    chunks=[]
    for offset in range(0,meta['bytes'],16000):
        code=f"import pathlib,json,base64\np=pathlib.Path({meta['path']!r});print(json.dumps(dict(data=base64.b64encode(p.read_bytes()[{offset}:{offset+16000}]).decode())))"
        chunk=c.rpc(environment,code,'fetch-'+label+'-'+str(offset))
        chunks.append(base64.b64decode(chunk['data']))
        print(label,offset+len(chunks[-1]),'/',meta['bytes'],flush=True)
    packed=b''.join(chunks)
    assert len(packed)==meta['bytes'] and hashlib.sha256(packed).hexdigest()==meta['sha256']
    (dest/'original-export.tar.gz').write_bytes(packed)
    with tarfile.open(fileobj=io.BytesIO(packed)) as tar:
        tar.extractall(dest,filter='data')
    for item in meta['files']:
        p=dest/item['path']
        assert p.stat().st_size==item['bytes'] and hashlib.sha256(p.read_bytes()).hexdigest()==item['sha256']
    (dest/'export-manifest.json').write_text(json.dumps(meta,indent=2)+'\n',encoding='utf-8')
    return dest


if __name__=='__main__':
    import argparse
    parser=argparse.ArgumentParser()
    parser.add_argument('environment')
    parser.add_argument('label')
    parser.add_argument('selection',nargs='+')
    args=parser.parse_args()
    fetch(args.environment,args.label,args.selection)
