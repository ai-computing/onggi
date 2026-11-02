import hashlib
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest

try:
    import causal
except ImportError:
    causal = None


class CausalTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(causal, 'causal harness is missing')

    def test_trace_counts_successful_returns_not_requested_lengths(self):
        trace = '''pread64(4</base/model>, "", 8, 0) = 3
pread64(4</base/model>, "", 8, 3) = -1 EIO (Input/output error)
pwrite64(5</out/model.onggi-partial>, "", 8, 0) = 2
pwrite64(5</out/model.onggi-partial>, "", 8, 2) = -1 ENOSPC (No space left on device)
ftruncate(5</out/model.onggi-partial>, 100000) = 0
fsync(5</out/model.onggi-partial>) = 0
read(6</cache/packs/ab>, "", 4096) = 7
read(7</store/manifests/ab>, "", 4096) = 11
'''
        got = causal.parse_trace(trace.splitlines(), {'base': '/base', 'target': '/out',
                                                     'cache': '/cache', 'store': '/store'})
        self.assertEqual(got['groups']['base']['read_bytes'], 3)
        self.assertEqual(got['groups']['base']['errors'], 1)
        self.assertEqual(got['groups']['target']['write_bytes'], 2)
        self.assertEqual(got['groups']['target']['errors'], 1)
        self.assertEqual(got['groups']['target']['ftruncate_calls'], 1)
        self.assertEqual(got['groups']['target']['fsync_calls'], 1)
        self.assertEqual(got['groups']['cache']['read_bytes'], 7)
        self.assertEqual(got['groups']['store']['read_bytes'], 11)
        self.assertTrue(got['valid'])

    def test_trace_resumes_threads_and_counts_copy_both_ends(self):
        trace = '''pwritev(5</out/f>, [{iov_base="", iov_len=4}], 1, 0 <unfinished ...>
<... pwritev resumed>) = 4
copy_file_range(4</base/f>, NULL, 5</out/f>, NULL, 20, 0) = 6
pwrite64(5</out/f>, "", 8, 4) = 8
'''
        got = causal.parse_trace(trace.splitlines(), {'base': '/base', 'target': '/out'})
        self.assertEqual(got['groups']['target']['write_bytes'], 18)
        self.assertEqual(got['groups']['base']['read_bytes'], 6)
        self.assertTrue(got['valid'])

    def test_pipe_copy_does_not_shift_destination_attribution(self):
        got = causal.parse_trace(['splice(4<pipe:[123]>, NULL, 5</out/f>, NULL, 20, 0) = 6'],
                                 {'target': '/out'})
        self.assertEqual(got['groups']['target']['write_bytes'], 6)
        self.assertEqual(got['groups']['target']['read_bytes'], 0)

    def test_gnu_time_keeps_blocks_distinct_from_bytes_and_rss(self):
        got = causal.time_metrics('User time (seconds): 1.23\nSystem time (seconds): 0.45\n'
                                  'Maximum resident set size (kbytes): 1234\nFile system inputs: 6\nFile system outputs: 8\n')
        self.assertEqual(got['user_s'], 1.23)
        self.assertEqual(got['system_s'], 0.45)
        self.assertEqual(got['max_rss_kib'], 1234)
        self.assertEqual(got['filesystem_output_blocks'], 8)

    def test_trace_roots_include_immutable_source_delta_and_scratch(self):
        cfg = dict(store='/store', base_dir='/immutable/a', target_dir='/immutable/b',
                   cache_dir='/cache-source', delta_file='/cohort/delta.vcdiff')
        roots = causal.trace_roots(cfg, pathlib.Path('/trial'))
        got = causal.parse_trace(['read(4</immutable/b/B8>, "", 9) = 9',
                                  'read(5</cohort/delta.vcdiff>, "", 7) = 7',
                                  'write(6</trial/tmp/onggi-cache-pack-x>, "", 3) = 3'], roots)
        self.assertEqual(got['groups']['source_target']['read_bytes'], 9)
        self.assertEqual(got['groups']['delta']['read_bytes'], 7)
        self.assertEqual(got['groups']['staging']['write_bytes'], 3)

    @unittest.skipUnless(sys.platform == 'linux' and shutil.which('strace'), 'Linux strace required')
    def test_command_confines_actual_temporary_file_io_to_trial_staging(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            row = causal.command(dict(gomaxprocs=32), [sys.executable, '-c',
                'import tempfile;f=tempfile.TemporaryFile();f.write(b"12345");f.seek(0);assert f.read()==b"12345"'],
                root, 'temp-file', trace=True)
            self.assertEqual(row['status'], 'succeeded')
            got = causal.read_traces(root / 'temp-file.trace', {'staging': str(root / 'tmp')})
            self.assertTrue(got['valid'], got)
            self.assertGreaterEqual(got['groups']['staging']['write_bytes'], 5)
            self.assertGreaterEqual(got['groups']['staging']['read_bytes'], 5)
            self.assertEqual(row['environment']['TMPDIR'], str(root / 'tmp'))

    def test_unsupported_mapped_io_and_incomplete_trace_fail_closed(self):
        got = causal.parse_trace(['mmap(NULL, 4096, PROT_READ, MAP_PRIVATE, 3</base/f>, 0) = 0x123',
                                  'pwrite64(4</out/f>, "", 8, 0 <unfinished ...>'],
                                 {'base': '/base', 'target': '/out'})
        self.assertFalse(got['valid'])
        self.assertIsNone(got['groups']['base']['read_bytes'])

    def test_independent_copy_rejects_symlinks_and_keeps_source_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            source = root / 'source'
            source.mkdir()
            (source / 'model').write_bytes(b'base')
            proof = causal.copy_base(source, root / 'copy')
            (root / 'copy/model').write_bytes(b'next')
            self.assertEqual((source / 'model').read_bytes(), b'base')
            self.assertTrue(proof[0]['independent_inode'])
            with self.assertRaises(FileExistsError):
                causal.copy_base(source, root / 'copy')
            if os.name != 'nt':
                (source / 'link').symlink_to('model')
                with self.assertRaises(ValueError):
                    causal.inventory(source)

    def test_failed_command_preserves_logs_and_status(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            row = causal.execute([sys.executable, '-c',
                                  'import sys;print("kept");sys.stderr.write("failed");sys.exit(7)'], root, 'attempt')
            self.assertEqual(row['exit_code'], 7)
            self.assertEqual(row['status'], 'failed')
            self.assertIn('kept', (root / 'attempt.stdout').read_text())
            self.assertEqual((root / 'attempt.stderr').read_text(), 'failed')
            self.assertIsNone(row['process_io'])

    def test_store_totals_separate_metadata_tmp_and_unknown_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            for name, content in [('packs/ab/p', b'12345'), ('index/ab/i', b'123'),
                                  ('refs/a', b'12'), ('tmp/pending', b'1')]:
                p = root / name
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_bytes(content)
            got = causal.store_totals(root)
            self.assertEqual(got['pack_bytes'], 5)
            self.assertEqual(got['metadata_bytes'], 5)
            self.assertEqual(got['finalized_bytes'], 10)
            self.assertEqual(got['tmp_bytes'], 1)
            (root / 'unexpected').write_bytes(b'x')
            with self.assertRaises(ValueError):
                causal.store_totals(root)

    def test_rsync_reports_both_directions_and_missing_is_unavailable(self):
        got = causal.rsync_stats('Total bytes sent: 1,234\nTotal bytes received: 56\n')
        self.assertEqual(got, {'sent_bytes': 1234, 'received_bytes': 56, 'protocol_bytes': 1290})
        self.assertIsNone(causal.rsync_stats('failed')['protocol_bytes'])

    def test_deploy_commands_hold_store_target_fixed_and_offer_only_r(self):
        cfg = dict(onggi='/bin/onggi', store='/frozen/store', base_manifest='sha256:' + 'a' * 64,
                   target_manifest='sha256:' + 'b' * 64, workers=8, target_dir='/source/target', delta_file='/delta.vcdiff',
                   target_inventory=[{'path': 'B8.rbln'}], base_inventory=[{'path': 'B2.rbln'}])
        plain = causal.deploy_argv(cfg, 'P', pathlib.Path('/trial'))
        reuse = causal.deploy_argv(cfg, 'R', pathlib.Path('/trial'))
        self.assertNotIn('--base', plain)
        self.assertEqual(reuse[reuse.index('--base') + 1], 'sha256:' + 'a' * 64)
        for argv in [plain, reuse]:
            self.assertEqual(argv[-2:], ['sha256:' + 'b' * 64, '/trial/output'])
            self.assertNotIn('--cache', argv)
            self.assertEqual(argv[argv.index('--store') + 1], '/frozen/store')
        rsync = causal.deploy_argv(cfg, 'rsync_inplace', pathlib.Path('/trial'))
        self.assertIn('--inplace', rsync)
        self.assertIn('--no-whole-file', rsync)
        self.assertNotIn('-z', rsync)
        self.assertEqual(rsync[-2:], ['/source/target/B8.rbln', '/trial/output/'])
        xdelta = causal.deploy_argv(cfg, 'xdelta', pathlib.Path('/trial'))
        self.assertEqual(xdelta[:4], ['xdelta3', '-d', '-s', '/trial/base/B2.rbln'])

    def test_publication_commands_match_codec_and_disable_frames_for_c_f_t(self):
        cfg = dict(onggi='/bin/onggi', workers=8)
        for arm in ['C', 'F', 'T']:
            argv = causal.publish_argv(cfg, arm, pathlib.Path('/trial'), 'a', '/source/a')
            self.assertEqual(argv[argv.index('--frame') + 1], '0')
            self.assertEqual(argv[argv.index('--compress') + 1], 'zstd')
            self.assertEqual(argv[argv.index('--store') + 1], '/trial/store')
        f = causal.publish_argv(cfg, 'F', pathlib.Path('/trial'), 'b', '/source/b')
        self.assertEqual(f[f.index('--avg') + 1], '4KiB')
        tf = causal.publish_argv(cfg, 'TF', pathlib.Path('/trial'), 'b', '/source/b')
        self.assertEqual(tf[tf.index('--frame') + 1], '256KiB')
        ta = causal.publish_argv(cfg, 'TA', pathlib.Path('/trial'), 'b', '/source/b')
        self.assertEqual(ta[ta.index('--compress') + 1], 'auto')

    def test_pair_order_alternates_and_rotates_publication(self):
        self.assertEqual(causal.orders(['P', 'R'], 5),
                         [['P', 'R'], ['R', 'P'], ['P', 'R'], ['R', 'P'], ['P', 'R']])
        self.assertEqual(causal.orders(['C', 'F', 'T'], 3),
                         [['C', 'F', 'T'], ['F', 'T', 'C'], ['T', 'C', 'F']])

    def test_extra_baselines_do_not_break_p_r_alternation(self):
        got = causal.orders(['P', 'R', 'rsync', 'rsync_inplace'], 5)
        self.assertEqual([[a for a in row if a in {'P', 'R'}] for row in got],
                         [['P', 'R'], ['R', 'P'], ['P', 'R'], ['R', 'P'], ['P', 'R']])

    def test_summary_preserves_failures_instead_of_aggregating_valid_subset(self):
        rows = [dict(arm='P', status='succeeded', valid=True, command={'elapsed_s': n},
                     validated_s=n + 1) for n in [1, 2, 3, 4, 100]]
        got = causal.summarize(rows, ['P'], 5)
        self.assertEqual(got['P']['command_s']['median'], 3)
        self.assertEqual(got['P']['command_s']['q1'], 2)
        self.assertEqual(got['P']['command_s']['q3'], 4)
        self.assertIsNone(causal.summarize(rows, ['P'], 5, cohort_valid=False)['P']['command_s'])
        rows[1] = dict(arm='P', status='failed', valid=False)
        got = causal.summarize(rows, ['P'], 5)
        self.assertFalse(got['P']['valid'])
        self.assertIsNone(got['P']['command_s'])
        self.assertEqual(len(got['P']['samples']), 5)

    @unittest.skipUnless(sys.platform == 'linux', 'Linux command runner required')
    def test_cli_publication_reaches_real_command_and_keeps_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            for name in ['source-a', 'source-b']:
                (root / name).mkdir()
                (root / name / 'f').write_bytes(b'fixture')
            # Failure sentinel tests dispatch/log retention, never simulates a valid Onggi result.
            tool = root / 'failure-sentinel'
            tool.write_text('#!/usr/bin/env python3\nimport sys\nsys.exit(0 if sys.argv[1:] == ["version"] else 9)\n')
            tool.chmod(0o755)
            cfg = dict(onggi=str(tool), binary_sha256=causal.sha(tool), harness_sha256=causal.sha(causal.__file__),
                       base_dir=str(root / 'source-a'), target_dir=str(root / 'source-b'),
                       base_inventory=causal.inventory(root / 'source-a'), target_inventory=causal.inventory(root / 'source-b'),
                       workers=8, gomaxprocs=32, publication_arms=['C'], store=None)
            causal.save(root / 'frozen.json', cfg)
            process = subprocess.run([sys.executable, causal.__file__, 'run', '--frozen', str(root / 'frozen.json'),
                                      '--experiment', 'publish', '--stage', 'smoke', '--output', str(root / 'run')])
            self.assertEqual(process.returncode, 1)
            row = json.loads((root / 'run/block-01-C/result.json').read_text())
            self.assertEqual(row['status'], 'failed')
            self.assertIn('command failed', row['error'])
            command = json.loads((root / 'run/block-01-C/push-a.json').read_text())
            self.assertEqual(command['exit_code'], 9)

    @unittest.skipUnless(os.environ.get('ONGGI_TEST_BIN'), 'set ONGGI_TEST_BIN for actual CLI fixture')
    def test_real_same_store_plain_consume_sha_and_trace(self):
        with tempfile.TemporaryDirectory() as tmp:
            got = causal.fixture(pathlib.Path(os.environ['ONGGI_TEST_BIN']).resolve(), pathlib.Path(tmp))
            self.assertTrue(got['valid'], got)
            self.assertEqual(got['plain_write_bytes'], 8 << 20)
            self.assertLess(got['consume_write_bytes'], 1 << 20)
            self.assertTrue(got['cached']['valid'], got)
            self.assertGreater(got['cached']['plain']['logical_io']['cache']['read_bytes'], 0)
            self.assertGreater(got['cached']['plain']['logical_io']['staging']['write_bytes'], 0)

    @unittest.skipUnless(sys.platform == 'linux' and shutil.which('strace'), 'Linux strace required')
    def test_actual_file_trace_duplicate_placements_moved_reads_and_hashes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            for name in ['base', 'target', 'cache']:
                (root / name).mkdir()
            (root / 'base/f').write_bytes(b'abcdefgh')
            (root / 'cache/f').write_bytes(b'IJ')
            trace = root / 'trace'
            subprocess.run(causal.trace_argv(trace) + [sys.executable, __file__, '--io-fixture', str(root)], check=True)
            got = causal.read_traces(trace, {k: str(root / k) for k in ['base', 'target', 'cache']})
            self.assertTrue(got['valid'], got)
            self.assertEqual(got['groups']['base']['read_bytes'], 10)  # 8 + short read of 2
            self.assertEqual(got['groups']['cache']['read_bytes'], 2)
            self.assertEqual(got['groups']['target']['write_bytes'], 24)  # 2 placements + vector + threads
            self.assertEqual((root / 'target/f').read_bytes(), b'abcdefghabcdefghIJKLLKXY')
            self.assertEqual(hashlib.sha256((root / 'base/f').read_bytes()).hexdigest(),
                             '9c56cc51b374c3ba189210d5b6d4bf57790d351c96c47c02190ecf1e430635ab')


def io_fixture(root):
    import concurrent.futures
    base = os.open(root / 'base/f', os.O_RDONLY)
    cache = os.open(root / 'cache/f', os.O_RDONLY)
    out = os.open(root / 'target/f', os.O_CREAT | os.O_RDWR, 0o600)
    data = os.pread(base, 8, 0)
    os.pread(base, 8, 6)
    os.pwrite(out, data, 0)
    os.pwrite(out, data, 8)
    os.pwritev(out, [os.pread(cache, 2, 0), b'KL'], 16)
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        list(pool.map(lambda pair: os.pwrite(out, pair[1], pair[0]), [(20, b'LK'), (22, b'XY')]))
    os.ftruncate(out, 24)
    os.fsync(out)
    for fd in [base, cache, out]:
        os.close(fd)


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] == '--io-fixture':
        io_fixture(pathlib.Path(sys.argv[2]))
    else:
        unittest.main()
