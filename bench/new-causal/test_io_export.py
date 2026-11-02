"""Trace attribution gates on small synthetic records, never model measurements."""
import copy
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import tarfile
import tempfile
import unittest

import causal
import verify_timing as v
import verify_io


class IoExportTests(unittest.TestCase):
    def test_export_excludes_model_payload_and_preserves_trace_bytes(self):
        import pack_trace_export
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / 'model'
            trial = source / 'io-smoke/block-01-P'
            (trial / 'output').mkdir(parents=True)
            (source / 'io-smoke/cohort.json').write_text('{"status":"succeeded"}')
            for name in ('preparation.json', 'io-config.json', 'io-frozen.json'):
                (source / name).write_text('{}')
            raw = b'pread64(4</cache/a>, "", 8, 0) = 8\r\n'
            (trial / 'deploy.trace.1').write_bytes(raw)
            (trial / 'result.json').write_text('{}')
            (trial / 'output/model.rbln').write_bytes(b'model-payload-must-not-export')
            (source / 'io-smoke/delta.vcdiff').write_bytes(b'no-delta-payload')
            package = root / 'package'
            manifest = pack_trace_export.pack(source, 'io-smoke', package)
            self.assertFalse(any('model.rbln' in f['path'] or 'vcdiff' in f['path'] for f in manifest['files']))
            extracted = root / 'extracted'
            extracted.mkdir()
            verify_io.extract_export(package, extracted, 'io-smoke')
            self.assertEqual((extracted / 'io-smoke/block-01-P/deploy.trace.1').read_bytes(), raw)
            with self.assertRaises(AssertionError):
                pack_trace_export.pack(source, 'io-smoke', package)

    def archive_fixture(self, directory, name='io-smoke/record.json'):
        root = Path(directory)
        data = b'{"fixture": true}\n'
        packed = io.BytesIO()
        with tarfile.open(fileobj=packed, mode='w:gz') as archive:
            info = tarfile.TarInfo(name)
            info.size = len(data)
            archive.addfile(info, io.BytesIO(data))
        payload = packed.getvalue()
        split = len(payload)//2
        parts = []
        for i, part in enumerate((payload[:split], payload[split:])):
            path = root / ('trace.tar.gz.part-%02d' % i)
            path.write_bytes(part)
            parts.append(dict(file=path.name, bytes=len(part), sha256=hashlib.sha256(part).hexdigest()))
        manifest = dict(schema='onggi.cpu-trace-export.v1', cohort='io-smoke',
            archive=dict(bytes=len(payload), sha256=hashlib.sha256(payload).hexdigest(), parts=parts),
            files=[dict(path=name, bytes=len(data), sha256=hashlib.sha256(data).hexdigest())])
        (root / 'export-manifest.json').write_text(json.dumps(manifest), encoding='utf-8')
        return root, data

    def test_split_archive_preserves_exact_trace_records(self):
        with tempfile.TemporaryDirectory() as directory:
            root, data = self.archive_fixture(directory)
            output = root / 'output'
            output.mkdir()
            result = verify_io.extract_export(root, output, 'io-smoke')
            self.assertEqual((output / 'io-smoke/record.json').read_bytes(), data)
            self.assertEqual(len(result['files']), 1)

    def test_archive_path_escape_and_modified_part_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root, _ = self.archive_fixture(directory, '../outside.json')
            output = root / 'output'
            output.mkdir()
            with self.assertRaises(AssertionError):
                verify_io.extract_export(root, output, 'io-smoke')
            self.assertFalse((root.parent / 'outside.json').exists())
        with tempfile.TemporaryDirectory() as directory:
            root, _ = self.archive_fixture(directory)
            part = root / 'trace.tar.gz.part-00'
            part.write_bytes(part.read_bytes() + b'bad')
            with self.assertRaises(AssertionError):
                verify_io.extract_export(root, root, 'io-smoke')

    def fixture(self, directory):
        trial = Path(directory)
        remote = PurePosixPath('/runs/io-smoke/block-01-P')
        cfg = dict(store='/store', base_dir='/inputs/base', target_dir='/inputs/target', cache_dir='/target-cache',
                   target_inventory=[dict(path='target.rbln', bytes=8)])
        (trial / 'deploy.trace.1').write_text(
            'pread64(4</runs/io-smoke/block-01-P/cache/packs/a>, "", 12, 0) = 8\n'
            'pwrite64(5</runs/io-smoke/block-01-P/output/target.rbln.onggi-partial>, "", 12, 0) = 8\n'
            'fsync(5</runs/io-smoke/block-01-P/output/target.rbln.onggi-partial>) = 0\n', encoding='utf-8')
        accounting = causal.read_traces(trial / 'deploy.trace', causal.trace_roots(cfg, remote))
        row = dict(arm='P', trace_accounting=accounting, logical_io=copy.deepcopy(accounting['groups']))
        return trial, remote, cfg, row

    def test_counts_reproduced_from_trace(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.fixture(directory)
            for arm in ('P', 'R'):
                args[-1]['arm'] = arm
                result = v.check_trace(*args, causal)
                self.assertEqual(result['target']['write_bytes'], 8)
                self.assertEqual(result['cache']['read_bytes'], 8)

    def test_false_reported_write_count_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            trial, remote, cfg, row = self.fixture(directory)
            row['logical_io']['target']['write_bytes'] = 0
            row['trace_accounting']['groups']['target']['write_bytes'] = 0
            with self.assertRaises(AssertionError):
                v.check_trace(trial, remote, cfg, row, causal)

    def test_r_wrong_run_and_empty_aggregate_cannot_report_zero_io(self):
        for trace in ('', 'pwrite64(5</wrong-run/output/target.rbln>, "", 8, 0) = 8\n'):
            with self.subTest(trace=trace), tempfile.TemporaryDirectory() as directory:
                trial, remote, cfg, row = self.fixture(directory)
                (trial / 'deploy.trace.1').write_text(trace, encoding='utf-8')
                row['arm'] = 'R'
                row['trace_accounting'] = causal.read_traces(trial / 'deploy.trace', causal.trace_roots(cfg, remote))
                row['logical_io'] = copy.deepcopy(row['trace_accounting']['groups'])
                with self.assertRaises(AssertionError):
                    v.check_trace(trial, remote, cfg, row, causal)

    def test_mapped_target_io_refuses_byte_claim(self):
        with tempfile.TemporaryDirectory() as directory:
            trial, remote, cfg, row = self.fixture(directory)
            with (trial / 'deploy.trace.1').open('a') as stream:
                stream.write('mmap(NULL, 4096, PROT_WRITE, MAP_SHARED, 5</runs/io-smoke/block-01-P/output/target.rbln>, 0) = 0x123\n')
            row['trace_accounting'] = causal.read_traces(trial / 'deploy.trace', causal.trace_roots(cfg, remote))
            row['logical_io'] = copy.deepcopy(row['trace_accounting']['groups'])
            with self.assertRaises(AssertionError):
                v.check_trace(trial, remote, cfg, row, causal)

    def test_missing_trace_and_unknown_root_reject_incomplete_accounting(self):
        with tempfile.TemporaryDirectory() as directory:
            trial, remote, cfg, row = self.fixture(directory)
            (trial / 'deploy.trace.1').unlink()
            with self.assertRaises(AssertionError):
                v.check_trace(trial, remote, cfg, row, causal)
            self.fixture(directory)
            wrong = PurePosixPath('/different-run/block-01-P')
            accounting = causal.read_traces(trial / 'deploy.trace', causal.trace_roots(cfg, wrong))
            row['trace_accounting'] = accounting
            row['logical_io'] = copy.deepcopy(accounting['groups'])
            with self.assertRaises(AssertionError):
                v.check_trace(trial, wrong, cfg, row, causal)

    def test_strace_command_flags_are_bound_to_gnu_time_record(self):
        command = dict(status='succeeded', exit_code=0, argv=['nice', '-n', '19', '/bin/true'], traced=True,
                       elapsed_s=.1, gnu_time='', resources={})
        resource = PurePosixPath('/run/deploy.resources')
        traced = causal.trace_argv(resource.with_suffix('.trace')) + command['argv']
        command['executed_argv'] = ['/usr/bin/time', '-v', '-o', str(resource)] + traced
        raw = 'Command being timed: "' + ' '.join(traced) + '"\nElapsed (wall clock) time (h:mm:ss or m:ss): 0:00.10\nExit status: 0\n'
        command['gnu_time'] = raw
        command['resources'] = causal.time_metrics(raw)
        v.check_command(command, ['/bin/true'], resource, raw, causal)
        command['executed_argv'].remove('-yy')
        with self.assertRaises(AssertionError):
            v.check_command(command, ['/bin/true'], resource, raw, causal)


if __name__ == '__main__':
    unittest.main()
