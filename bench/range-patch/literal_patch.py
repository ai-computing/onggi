"""Benchmark-only prepared raw-literal range patch, not a production loader."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import time

SCHEMA = 'onggi.bench.literal-patch.v1'
BUFFER = 1 << 20
MAX_HEADER = 64 << 20
MAX_FILE = 1 << 40


class ApplyFailure(RuntimeError):
    def __init__(self, error, work_file):
        super().__init__(str(error))
        self.work_file = work_file


def regular(path, *, single=False):
    path = Path(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or (single and info.st_nlink != 1):
        raise ValueError('expected an ordinary unaliased file' if single else 'expected a regular non-symlink file')
    return path.resolve(strict=True), info


def sha_file(path):
    digest, count = hashlib.sha256(), 0
    with path.open('rb') as stream:
        while data := stream.read(BUFFER):
            digest.update(data)
            count += len(data)
    return digest.hexdigest(), count


def keep_plan(ranges, base_size, target_size):
    if not isinstance(ranges, list) or len(ranges) > 1000000:
        raise ValueError('invalid keep plan')
    end = 0
    for row in ranges:
        if not isinstance(row, list) or len(row) != 2 or any(type(n) is not int for n in row):
            raise ValueError('keep entries must be integer offset/length pairs')
        offset, length = row
        if offset < end or length <= 0 or offset + length > min(base_size, target_size):
            raise ValueError('invalid or overlapping keep range')
        end = offset + length
    return ranges


def residuals(keep, size):
    end, result = 0, []
    for offset, length in keep:
        if offset > end:
            result.append((end, offset-end))
        end = offset+length
    if end < size:
        result.append((end, size-end))
    return result


def intervals(keep, size):
    return sorted([(offset, length, True) for offset, length in keep]
                  + [(offset, length, False) for offset, length in residuals(keep, size)])


def prepare(base, target, keep_ranges, output):
    started = time.perf_counter()
    base, bi = regular(base)
    target, ti = regular(target)
    if max(bi.st_size, ti.st_size) > MAX_FILE:
        raise ValueError('reference file size limit exceeded')
    keep = keep_plan(keep_ranges, bi.st_size, ti.st_size)
    output = Path(output).resolve()
    output.mkdir(exist_ok=False)
    base_sha, base_reads = sha_file(base)
    target_sha, target_reads = sha_file(target)
    literals, payload_hash, target_check = [], hashlib.sha256(), hashlib.sha256()
    payload_bytes = 0
    with base.open('rb') as old, target.open('rb') as new, (output/'payload.bin').open('xb') as payload:
        for offset, length, same in intervals(keep, ti.st_size):
            new.seek(offset)
            old.seek(offset)
            part_hash, remaining = hashlib.sha256(), length
            while remaining:
                data = new.read(min(BUFFER, remaining))
                if not data:
                    raise ValueError('target changed during preparation')
                target_reads += len(data)
                target_check.update(data)
                if same:
                    before = old.read(len(data))
                    base_reads += len(before)
                    if before != data:
                        raise ValueError('untruthful same-offset keep range')
                else:
                    if payload.write(data) != len(data):
                        raise OSError('short payload write')
                    part_hash.update(data)
                    payload_hash.update(data)
                remaining -= len(data)
            if not same:
                literals.append(dict(offset=offset, length=length, payload_offset=payload_bytes, sha256=part_hash.hexdigest()))
                payload_bytes += length
        if target_check.hexdigest() != target_sha or new.read(1):
            raise ValueError('target changed during preparation')
        payload.flush()
        os.fsync(payload.fileno())
    manifest = dict(schema=SCHEMA, base_sha256=base_sha, base_bytes=bi.st_size,
                    target_sha256=target_sha, target_bytes=ti.st_size, keep_ranges=keep,
                    literals=literals, payload_bytes=payload_bytes, payload_sha256=payload_hash.hexdigest())
    header = (json.dumps(manifest, separators=(',', ':'))+'\n').encode()
    if len(header) > MAX_HEADER:
        raise ValueError('manifest limit exceeded; failed package retained')
    with (output/'manifest.json').open('xb') as stream:
        stream.write(header)
        stream.flush()
        os.fsync(stream.fileno())
    return dict(literal_bytes=payload_bytes, kept_bytes=sum(row[1] for row in keep),
                patch_bytes=payload_bytes+len(header), base_read_bytes=base_reads,
                target_read_bytes=target_reads, preparation_seconds=time.perf_counter()-started,
                target_sha256=target_sha, manifest_sha256=hashlib.sha256(header).hexdigest(),
                payload_sha256=payload_hash.hexdigest(), base_sha256=base_sha)


def unique_object(pairs):
    result = dict(pairs)
    if len(result) != len(pairs):
        raise ValueError('duplicate JSON key')
    return result


def checked_patch(patch, identity):
    patch = Path(patch)
    if patch.is_symlink() or not patch.is_dir():
        raise ValueError('expected patch directory')
    header, info = regular(patch/'manifest.json')
    payload, pi = regular(patch/'payload.bin')
    if info.st_size > MAX_HEADER:
        raise ValueError('manifest limit exceeded')
    raw = header.read_bytes()
    if not isinstance(identity, str) or hashlib.sha256(raw).hexdigest() != identity:
        raise ValueError('manifest identity differs from admitted publication')
    doc = json.loads(raw, object_pairs_hook=unique_object)
    keys = {'schema', 'base_sha256', 'base_bytes', 'target_sha256', 'target_bytes',
            'keep_ranges', 'literals', 'payload_bytes', 'payload_sha256'}
    if not isinstance(doc, dict) or set(doc) != keys or doc['schema'] != SCHEMA:
        raise ValueError('unsupported manifest')
    for name in ('base_bytes', 'target_bytes', 'payload_bytes'):
        if type(doc[name]) is not int or not 0 <= doc[name] <= MAX_FILE:
            raise ValueError('invalid byte count')
    for name in ('base_sha256', 'target_sha256', 'payload_sha256'):
        if not isinstance(doc[name], str) or not re.fullmatch('[0-9a-f]{64}', doc[name]):
            raise ValueError('invalid digest')
    keep_plan(doc['keep_ranges'], doc['base_bytes'], doc['target_bytes'])
    if not isinstance(doc['literals'], list) or len(doc['literals']) > 1000001:
        raise ValueError('invalid literals')
    expected = residuals(doc['keep_ranges'], doc['target_bytes'])
    if len(expected) != len(doc['literals']):
        raise ValueError('literal coverage differs')
    position, whole = 0, hashlib.sha256()
    with payload.open('rb') as stream:
        for row, pair in zip(doc['literals'], expected):
            if (not isinstance(row, dict) or set(row) != {'offset', 'length', 'payload_offset', 'sha256'}
                    or any(type(row.get(key)) is not int for key in ('offset', 'length', 'payload_offset'))
                    or (row['offset'], row['length']) != pair or row['payload_offset'] != position):
                raise ValueError('literal structure differs')
            digest, remaining = hashlib.sha256(), row['length']
            while remaining:
                data = stream.read(min(BUFFER, remaining))
                if not data:
                    raise ValueError('truncated payload')
                digest.update(data)
                whole.update(data)
                remaining -= len(data)
            if digest.hexdigest() != row['sha256']:
                raise ValueError('literal digest differs')
            position += row['length']
        if (stream.read(1) or position != pi.st_size or position != doc['payload_bytes']
                or whole.hexdigest() != doc['payload_sha256']):
            raise ValueError('payload identity differs')
    return doc, payload


def apply(base, patch, output, *, manifest_sha256, exclusive=False):
    started = time.perf_counter()
    if not exclusive:
        raise ValueError('caller must admit exclusive ownership of the private base and output parent')
    base, info = regular(base, single=True)
    output = Path(output)
    if os.path.lexists(output):
        raise FileExistsError('output must be new')
    output = output.parent.resolve(strict=True)/output.name
    package = Path(patch).resolve(strict=True)
    if base.is_relative_to(package) or output.is_relative_to(package) or base == output:
        raise ValueError('source, patch and output paths alias')
    if output.parent.stat().st_dev != info.st_dev:
        raise ValueError('reference requires one filesystem')
    doc, payload = checked_patch(patch, manifest_sha256)
    base_sha, base_reads = sha_file(base)
    if base_reads != doc['base_bytes'] or base_sha != doc['base_sha256']:
        raise ValueError('base identity differs')
    validated = time.perf_counter()
    descriptor, temporary = tempfile.mkstemp(prefix='.literal-patch-', dir=output.parent)
    os.close(descriptor)
    work = Path(temporary)
    try:
        os.replace(base, work)
        with work.open('r+b') as destination, payload.open('rb') as source:
            for row in doc['literals']:
                destination.seek(row['offset'])
                source.seek(row['payload_offset'])
                remaining = row['length']
                while remaining:
                    data = source.read(min(BUFFER, remaining))
                    if not data or destination.write(data) != len(data):
                        raise OSError('short literal read/write')
                    remaining -= len(data)
            destination.truncate(doc['target_bytes'])
            destination.flush()
            os.fsync(destination.fileno())
        target_sha, target_reads = sha_file(work)
        if target_reads != doc['target_bytes'] or target_sha != doc['target_sha256']:
            raise ValueError('constructed target differs')
        os.link(work, output)  # Exclusive publication cannot overwrite a racing destination.
        published = time.perf_counter()
    except (OSError, ValueError) as error:
        raise ApplyFailure(error, work) from error
    cleanup_error = None
    try:
        work.unlink()
    except OSError as error:
        cleanup_error = str(error)
    finished = time.perf_counter()
    return dict(kept_bytes=sum(row[1] for row in doc['keep_ranges']),
                literal_written_bytes=doc['payload_bytes'], literal_read_bytes=doc['payload_bytes'],
                payload_validation_read_bytes=doc['payload_bytes'], base_read_bytes=base_reads,
                target_validation_read_bytes=target_reads, target_sha256=target_sha,
                validation_before_mutation_seconds=validated-started,
                mutation_to_publication_seconds=published-validated,
                post_publication_cleanup_seconds=finished-published,
                application_seconds=finished-started, manifest_sha256=manifest_sha256,
                cleanup_error=cleanup_error, retained_work_file=str(work) if cleanup_error else None)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='operation', required=True)
    for name in ('prepare', 'apply'):
        command = commands.add_parser(name)
        command.add_argument('--base', type=Path, required=True)
        command.add_argument('--output', type=Path, required=True)
        if name == 'prepare':
            command.add_argument('--target', type=Path, required=True)
            command.add_argument('--keep', type=Path, required=True)
        else:
            command.add_argument('--patch', type=Path, required=True)
            command.add_argument('--manifest-sha256', required=True)
            command.add_argument('--exclusive', action='store_true')
    args = parser.parse_args()
    try:
        if args.operation == 'prepare':
            if args.keep.stat().st_size > MAX_HEADER:
                raise ValueError('keep plan limit exceeded')
            result = prepare(args.base, args.target, json.loads(args.keep.read_bytes(), object_pairs_hook=unique_object), args.output)
        else:
            result = apply(args.base, args.patch, args.output, manifest_sha256=args.manifest_sha256, exclusive=args.exclusive)
        print(json.dumps(dict(status='completed', operation=args.operation,
                              source_sha256=sha_file(Path(__file__))[0], result=result)))
        return 0
    except (OSError, ValueError, ApplyFailure) as error:
        print(json.dumps(dict(status='failed', operation=args.operation, error=str(error),
                              retained_work_file=str(error.work_file) if isinstance(error, ApplyFailure) else None)), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
