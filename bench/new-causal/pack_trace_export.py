"""Stream exact terminal-cohort evidence into bounded compressed transport parts.

Read-only source, fresh output directory; model/output/store trees are excluded.
Run after timing/publication work to avoid export I/O in a measured interval.
"""
import argparse
import hashlib
import json
from pathlib import Path
import tarfile


class HashedReader:
    def __init__(self, stream):
        self.stream, self.count, self.digest = stream, 0, hashlib.sha256()

    def read(self, size):
        block = self.stream.read(size)
        self.count += len(block)
        self.digest.update(block)
        return block


def pack(source, label, output):
    source, output = source.resolve(), output.resolve()
    assert label in ('io-smoke', 'io-main')
    assert not output.exists() and not output.is_relative_to(source) and not source.is_relative_to(output)
    cohort = source / label
    assert json.loads((cohort / 'cohort.json').read_bytes())['status'] in ('succeeded', 'failed')
    suffixes = {'.json', '.py', '.stdout', '.stderr', '.resources'}
    paths = [p for p in cohort.iterdir() if p.is_file() and p.suffix in suffixes]
    for trial in cohort.glob('block-*'):
        if trial.is_dir():
            paths += [p for p in trial.iterdir() if p.is_file() and (p.suffix in suffixes or '.trace.' in p.name)]
    paths += [source / name for name in ('preparation.json', 'io-config.json', 'io-frozen.json')]
    paths = sorted(set(paths))
    assert all(p.is_file() and not p.is_symlink() and p.resolve().is_relative_to(source) for p in paths)
    output.mkdir(parents=True)
    archive_path = output / 'evidence.tar.gz'
    rows = []
    with tarfile.open(archive_path, mode='w:gz') as archive:
        for path in paths:
            before = path.stat()
            name = path.relative_to(source).as_posix()
            info = tarfile.TarInfo(name)
            info.size, info.mode = before.st_size, 0o644
            with path.open('rb') as stream:
                reader = HashedReader(stream)
                archive.addfile(info, reader)
            after = path.stat()
            assert (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) == (
                after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns), 'Evidence changed during export'
            assert reader.count == before.st_size
            rows.append(dict(path=name, bytes=reader.count, sha256=reader.digest.hexdigest()))
    parts, total, digest = [], 0, hashlib.sha256()
    with archive_path.open('rb') as stream:
        for index, block in enumerate(iter(lambda: stream.read(48_000_000), b'')):
            name = 'evidence.tar.gz.part-%03d' % index
            (output / name).write_bytes(block)
            parts.append(dict(file=name, bytes=len(block), sha256=hashlib.sha256(block).hexdigest()))
            digest.update(block)
            total += len(block)
    manifest = dict(schema='onggi.cpu-trace-export.v1', cohort=label, files=rows,
                    archive=dict(bytes=total, sha256=digest.hexdigest(), parts=parts),
                    scope='Exact evidence bytes; successful syscall lengths, not physical I/O. Large payload trees excluded.')
    (output / 'export-manifest.json').write_text(json.dumps(manifest, indent=2)+'\n', encoding='utf-8')
    return manifest


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--label', choices=['io-smoke', 'io-main'], required=True)
    args = parser.parse_args()
    result = pack(args.source, args.label, args.output)
    print(json.dumps(dict(label=args.label, files=len(result['files']), archive=result['archive'])))
