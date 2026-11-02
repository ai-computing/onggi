"""Separate same-cache syscall diagnostic; never merge its times with untraced trials."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import statistics
import tarfile
import tempfile

import verify_timing as v


def extract_export(root, output, label):
    manifest = json.loads((root / 'export-manifest.json').read_bytes())
    assert manifest['schema'] == 'onggi.cpu-trace-export.v1' and manifest['cohort'] == label
    payload = []
    for part in manifest['archive']['parts']:
        assert Path(part['file']).name == part['file'] and '\\' not in part['file']
        data = (root / part['file']).read_bytes()
        assert len(data) == part['bytes'] and v.digest(data) == part['sha256']
        payload.append(data)
    packed = b''.join(payload)
    assert len(packed) == manifest['archive']['bytes'] and v.digest(packed) == manifest['archive']['sha256']
    expected = {row['path']: row for row in manifest['files']}
    assert expected and len(expected) == len(manifest['files'])
    seen = set()
    with tarfile.open(fileobj=io.BytesIO(packed), mode='r|gz') as archive:
        for entry in archive:
            assert entry.isfile() and entry.name in expected and entry.name not in seen
            destination = (output / entry.name).resolve()
            assert destination.is_relative_to(output.resolve()) and '\\' not in entry.name
            row = expected[entry.name]
            assert entry.size == row['bytes']
            destination.parent.mkdir(parents=True, exist_ok=True)
            count, digest = 0, hashlib.sha256()
            with archive.extractfile(entry) as source, destination.open('xb') as target:
                for block in iter(lambda: source.read(1 << 20), b''):
                    count += len(block)
                    digest.update(block)
                    target.write(block)
            assert count == row['bytes'] and digest.hexdigest() == row['sha256']
            seen.add(entry.name)
    assert seen == set(expected)
    return manifest


def verify(root, label):
    # Compress once for transport/Git; temporary raw traces are never committed.
    with tempfile.TemporaryDirectory(prefix='onggi-io-verify-') as directory:
        output = Path(directory)
        manifest = extract_export(root, output, label)
        result = v._verify_contents(output, label, True, len(manifest['files']))
        cohort = json.loads((output / label / 'cohort.json').read_bytes())
    groups = {}
    for arm in cohort['arms']:
        rows = [row for row in cohort['trials'] if row['arm'] == arm]
        groups[arm] = {}
        for location in rows[0]['logical_io']:
            groups[arm][location] = {}
            for metric in ('read_bytes', 'write_bytes', 'fsync_calls', 'ftruncate_calls'):
                values = [row['logical_io'][location][metric] for row in rows]
                q1, _, q3 = statistics.quantiles(values, method='inclusive') if len(values) > 1 else [values[0]] * 3
                groups[arm][location][metric] = dict(values=values, median=statistics.median(values), q1=q1, q3=q3)
    result.update(logical_io=groups, physical_io=None,
                  io_scope='Successful syscall return lengths during traced deployment command only; sync/fullSHA outside traces',
                  archive_sha256=manifest['archive']['sha256'])
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('label', choices=['io-smoke', 'io-main'])
    parser.add_argument('--root', type=Path, default=Path(__file__).parent / 'data/20261102')
    args = parser.parse_args()
    result = verify(args.root / args.label, args.label)
    result['control'] = v.verify_controls(args.root / 'control', [
        'cdx-resident-cpu-io-smoke', 'cdx-resident-cpu-io-main'])
    result['provenance'] = v.verify_provenance(args.root)
    print(json.dumps(result, indent=2))
