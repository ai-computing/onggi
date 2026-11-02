"""Build a labeled benchmark reference in a new private source tree.

The sole change admits a provably identity sub-split. No product file is edited.
"""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import subprocess
import sys

BASE='1e3c74f3c61f8791eadbec704b17df10feb55241'
TARGET='pkg/store/store.go'
OLD=b'if c.Sub.Max > p.Min {'
NEW=b'if c.Sub.Max > p.Min && c.Sub.Min < p.Max {'


def patch_store_source(raw):
    if raw.count(OLD)!=1:raise ValueError('Reference patch requires exact single reviewed guard')
    return raw.replace(OLD,NEW,1)


def digest(raw):return hashlib.sha256(raw).hexdigest()


def generate(repo,output):
    repo=Path(repo).resolve();output=Path(output).resolve()
    if output==repo or repo in output.parents or output in repo.parents:
        raise ValueError('Reference output must be disjoint from product checkout')
    output.mkdir(parents=True,exist_ok=False)
    names=subprocess.check_output(['git','ls-tree','-r','--name-only',BASE,'--','cmd','pkg','go.mod','go.sum'],cwd=repo).decode().splitlines()
    rows=[];source=output/'source';source.mkdir()
    for name in names:
        p=PurePosixPath(name)
        if p.is_absolute() or '..' in p.parts:raise ValueError('Invalid Git source path')
        raw=subprocess.check_output(['git','show',BASE+':'+name],cwd=repo)
        changed=patch_store_source(raw) if name==TARGET else raw
        path=source/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(changed)
        rows.append(dict(path=name,base_bytes=len(raw),base_sha256=digest(raw),
            reference_bytes=len(changed),reference_sha256=digest(changed)))
    modified=[r['path'] for r in rows if r['base_sha256']!=r['reference_sha256']]
    if modified!=[TARGET]:raise ValueError('Reference unexpectedly changes other product source')
    record=dict(schema='onggi.flat-frame-reference.v1',status='generated-not-built',base_revision=BASE,
        generator_sha256=digest(Path(__file__).read_bytes()),
        product_source_edited=False,reference_source=str(source),files=rows,modified=modified,
        patch=dict(path=TARGET,before=OLD.decode(),after=NEW.decode()),
        meaning='Identity sub-split only: sub minimum >= top-level maximum. Experimental reference, not unchanged production capability.')
    (output/'reference-manifest.json').write_text(json.dumps(record,indent=2)+'\n')
    return record


def build(repo,output):
    output=Path(output).resolve();record=generate(repo,output);source=Path(record['reference_source'])
    commands=[['go','version'],['go','vet','./...'],['go','test','-race','./...'],
        ['go','build','-buildvcs=false','-trimpath','-ldflags','-s -w','-o',str(output/'onggi'),'./cmd/onggi']]
    record['commands']=[]
    for i,argv in enumerate(commands):
        run=subprocess.run(argv,cwd=source,capture_output=True)
        (output/('command-%d.stdout'%i)).write_bytes(run.stdout)
        (output/('command-%d.stderr'%i)).write_bytes(run.stderr)
        record['commands'].append(dict(argv=argv,exit_code=run.returncode,
            stdout_sha256=digest(run.stdout),stderr_sha256=digest(run.stderr)))
        record['status']='failed' if run.returncode else 'building'
        (output/'reference-manifest.json').write_text(json.dumps(record,indent=2)+'\n')
        if run.returncode:raise RuntimeError('Reference build/check failed; logs retained')
    record.update(status='built',binary=str(output/'onggi'),binary_sha256=digest((output/'onggi').read_bytes()))
    (output/'reference-manifest.json').write_text(json.dumps(record,indent=2)+'\n')
    return record


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--repo',type=Path,required=True)
    p.add_argument('--output',type=Path,required=True);p.add_argument('--generate-only',action='store_true');a=p.parse_args()
    print(json.dumps((generate if a.generate_only else build)(a.repo,a.output)))
