"""Public legacy FileDiff to same-offset keep plan; benchmark preparation only."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import sys
import time

import literal_patch as patch


def require(condition, message):
    if not condition:
        raise ValueError(message)


def artifact_path(value):
    require(isinstance(value, str) and value and '\\' not in value
            and not value.startswith('/') and all(p not in ('', '.', '..') for p in value.split('/'))
            and str(PurePosixPath(value)) == value, 'expected exact relative artifact path')
    return value


def derive_keep(document, base_path, target_path, base_size, target_size):
    artifact_path(base_path)
    artifact_path(target_path)
    require(all(type(n) is int and 0 <= n <= patch.MAX_FILE for n in [base_size, target_size]),
            'invalid actual file size')
    require(isinstance(document, dict) and 'schema' not in document,
            'only the measured legacy FileDiff format is supported')
    files = document.get('files')
    require(isinstance(files, list) and all(isinstance(f, dict) for f in files), 'missing file diff list')
    paths = [artifact_path(f.get('path')) for f in files]
    require(len(set(paths)) == len(paths) and target_path in paths, 'missing or duplicate target file')
    row = files[paths.index(target_path)]
    require(row.get('base') == base_path and row.get('size') == target_size,
            'diff pair or size differs from selected immutable inputs')
    values = [row.get(k) for k in ['size', 'same', 'moved', 'new', 'changed_runs', 'largest_run']]
    require(all(type(n) is int and 0 <= n <= patch.MAX_FILE for n in values), 'invalid diff counter')
    size, same, moved, new, runs, largest = values
    require(same+moved+new == size, 'diff byte counters do not partition target')
    changed = patch.keep_plan(row.get('changed', []), size, size)
    require(len(changed) == runs and sum(r[1] for r in changed) == moved+new
            and max((r[1] for r in changed), default=0) == largest,
            'missing --ranges or inconsistent changed-range coverage')
    keep = [list(r) for r in patch.residuals(changed, size)]
    patch.keep_plan(keep, base_size, size)
    require(sum(r[1] for r in keep) == same, 'same-offset coverage differs')
    return keep


def prepare(base, target, diff, output, *, diff_sha256, base_path, target_path,
            base_sha256, target_sha256):
    started = time.perf_counter()
    for digest in [diff_sha256, base_sha256, target_sha256]:
        require(isinstance(digest, str) and re.fullmatch('[0-9a-f]{64}', digest) is not None,
                'an independently frozen SHA256 pin is required')
    base, bi = patch.regular(base)
    target, ti = patch.regular(target)
    diff, di = patch.regular(diff)
    require(di.st_size <= patch.MAX_HEADER and max(bi.st_size, ti.st_size) <= patch.MAX_FILE,
            'reference input limit exceeded')
    raw = diff.read_bytes()
    require(hashlib.sha256(raw).hexdigest() == diff_sha256, 'diff identity differs from frozen input')
    document = json.loads(raw, object_pairs_hook=patch.unique_object)
    keep = derive_keep(document, base_path, target_path, bi.st_size, ti.st_size)
    base_hash, base_reads = patch.sha_file(base)
    target_hash, target_reads = patch.sha_file(target)
    require((base_hash, target_hash) == (base_sha256, target_sha256), 'complete input identity differs')
    verified = time.perf_counter()
    prepared = patch.prepare(base, target, keep, output)
    require((prepared['base_sha256'], prepared['target_sha256']) == (base_sha256, target_sha256),
            'immutable preparation inputs changed')
    output = Path(output)
    keep_bytes = (json.dumps(keep, separators=(',', ':'))+'\n').encode()
    with (output/'keep.json').open('xb') as stream:
        stream.write(keep_bytes)
    receipt = dict(schema='onggi.bench.diff-keep-preparation.v1',
                   diff_format='legacy FileDiff; onggi diff --ranges --json',
                   diff_sha256=diff_sha256, base_path=base_path, target_path=target_path,
                   base_sha256=base_sha256, target_sha256=target_sha256,
                   keep_sha256=hashlib.sha256(keep_bytes).hexdigest(),
                   adapter_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                   reference_sha256=hashlib.sha256(Path(patch.__file__).read_bytes()).hexdigest(),
                   input_verification_read_bytes=base_reads+target_reads,
                   input_verification_seconds=verified-started,
                   preparation_total_seconds=time.perf_counter()-started,
                   preparation_endpoint='keep file written; excludes receipt serialization/write',
                   evidence_scope='patch preparation only; no application or model latency result',
                   patch=prepared)
    with (output/'preparation.json').open('x', encoding='utf-8') as stream:
        json.dump(receipt, stream, indent=2)
        stream.write('\n')
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['base', 'target', 'diff', 'output', 'diff-sha256', 'base-path',
                 'target-path', 'base-sha256', 'target-sha256']:
        parser.add_argument('--'+name, required=True)
    args = vars(parser.parse_args())
    try:
        print(json.dumps(prepare(**args), indent=2))
    except Exception as error:
        print(json.dumps(dict(status='failed', error=str(error))), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
