"""Read-only original artifact check, off-card, before Jobs start."""
import base64
import json
import pathlib
import subprocess
import time

from common import file_sha, save

ROOT = pathlib.Path('/home/gspark/workspace/codex_e11_20261031')
OLD = pathlib.Path('/home/gspark/workspace/codex_ccgrid_npu_20261030')
proof_path = OLD / 'evidence/e6-input-proof-v2.json'
proof = json.loads(proof_path.read_text())
artifacts = {}
hashed = {}
for role, artifact in zip(['base', 'target'], proof['artifacts']):
    root = pathlib.Path(artifact['path'])
    files = []
    for item in artifact['files']:
        p = root / item['name']
        st = p.stat()
        key = (st.st_dev, st.st_ino, st.st_size, st.st_mtime_ns)
        if key not in hashed:
            hashed[key] = file_sha(p)
        assert st.st_size == item['bytes'] and hashed[key] == item['sha256'], str(p)
        files.append(dict(path=item['name'], bytes=st.st_size, sha256=hashed[key]))
    manifest = artifact['manifest']
    digest = manifest.split(':')[1]
    mp = OLD / 'store/e6-posix/manifests' / digest[:2] / digest
    assert file_sha(mp) == digest
    artifacts[role] = dict(manifest=manifest, files=files, manifest_json=json.loads(mp.read_text()))
configs = []
for p in (OLD / 'store/e6-posix/config').rglob('*'):
    if p.is_file():
        configs.append(dict(path=p.relative_to(OLD/'store/e6-posix').as_posix(),
                            base64=base64.b64encode(p.read_bytes()).decode(), sha256=file_sha(p)))
assert len(configs) == 1
save(ROOT / 'inputs.json', dict(schema='onggi.e11.inputs.v1', utc=time.time(),
     source_commit='05c97605b07882f9ceaaeb90b94878f8923f81cf',
     artifacts=artifacts, store_config=configs,
     prior_proof_sha256=file_sha(proof_path), prior_proof=proof,
     tools={p.name: dict(bytes=p.stat().st_size, sha256=file_sha(p))
            for p in [ROOT/'onggi', ROOT/'versitygw', ROOT/'gateway.tar.gz']},
     go_version=subprocess.check_output(['/home/gspark/.cache/onggi-build/go/bin/go','version'],text=True).strip()))
print('full input hashes verified', flush=True)
