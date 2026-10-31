"""Cluster-internal origin proxy, central barrier and raw trial recorder."""
import argparse
import collections
import http.client
import json
import pathlib
import subprocess
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

from common import BodyCounter, file_sha, save, validate_cohort, validate_ready

ROOT = pathlib.Path('/e11')
LOCK = threading.RLock()
STATE = dict(workers={}, trial=None, boot=time.time(), counters=BodyCounter(), active_proxy=0, stop=False)
NODES = ['cpu-1', 'cpu-2', 'etri-001']


def nic_info():
    out = {}
    for p in pathlib.Path('/host-sys/class/net').glob('*'):
        fields = {}
        for name in ['operstate', 'speed', 'duplex', 'mtu', 'statistics/rx_bytes', 'statistics/tx_bytes']:
            try:
                fields[name] = (p/name).read_text().strip()
            except OSError as e:
                fields[name] = type(e).__name__
        out[p.name] = fields
    return out


def public_state():
    with LOCK:
        t = STATE['trial']
        return dict(workers=STATE['workers'], trial=None if t is None else
                    {k: v for k, v in t.items() if k not in ['commands', 'counter']},
                    origin_nics=nic_info())


def finish_trial(trial):
    try:
        deadline = time.monotonic()+1800
        while True:
            with LOCK:
                if set(trial['ready']) == set(NODES) and not STATE['active_proxy']:
                    if trial['results']:
                        raise RuntimeError('worker failed before barrier')
                    inputs=json.loads((ROOT/'inputs.json').read_text())
                    validate_ready(trial,inputs,STATE['workers'][NODES[0]]['seed_cache_inventory'])
                    trial['preparation_origin'] = STATE['counters'].snapshot()
                    STATE['counters'] = BodyCounter()
                    trial['release_mono'] = time.monotonic()
                    trial['release_utc'] = time.time()
                    trial['origin_nics_before'] = nic_info()
                    break
                if trial['results']:
                    raise RuntimeError('worker failed before barrier')
            if time.monotonic() > deadline:
                raise TimeoutError('ready barrier timed out')
            time.sleep(.02)
        while True:
            with LOCK:
                if set(trial['results']) == set(NODES) and not STATE['active_proxy']:
                    trial['status'] = 'completed'
                    trial['origin'] = STATE['counters'].snapshot()
                    trial['origin_nics_after'] = nic_info()
                    trial['metrics'] = validate_cohort(trial)
                    break
            if time.monotonic() > deadline:
                raise TimeoutError('cohort completion timed out')
            time.sleep(.02)
    except BaseException:
        with LOCK:
            trial['status'] = 'failed'
            trial['error'] = traceback.format_exc()
            trial['origin'] = STATE['counters'].snapshot()
    finally:
        with LOCK:
            trial['finished_utc'] = time.time()
            save(ROOT/'runs'/trial['trial']/'cohort.json',
                 {k: v for k, v in trial.items() if k != 'commands'})


class Admin(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, obj, status=200):
        raw = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        path = urlsplit(self.path).path
        if path == '/status':
            return self.reply(public_state())
        if path in ['/assets/onggi', '/inputs.json']:
            p = ROOT/('onggi' if path.startswith('/assets/') else 'inputs.json')
            self.send_response(200)
            self.send_header('Content-Length', str(p.stat().st_size))
            self.end_headers()
            with p.open('rb') as f:
                while b := f.read(1 << 20):
                    self.wfile.write(b)
            return
        if path.startswith('/command/'):
            node = path.rsplit('/', 1)[1]
            with LOCK:
                trial = STATE['trial']
                cmd = dict(stop=True) if STATE['stop'] else trial['commands'].get(node) if trial else None
            return self.reply(cmd)
        if path.startswith('/barrier/'):
            name = path.rsplit('/', 1)[1]
            with LOCK:
                t = STATE['trial']
                return self.reply(dict(released=bool(t and t['trial'] == name and 'release_mono' in t),
                                       failed=bool(t and t['trial'] == name and t['status'] == 'failed')))
        return self.reply(dict(error='unknown route'), 404)

    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))))
        path = self.path
        with LOCK:
            if path == '/stop':
                STATE['stop'] = True
            elif path == '/shutdown':
                STATE['stop'] = True
                threading.Thread(target=self.server.shutdown,daemon=True).start()
            elif path == '/register':
                assert data['node'] in NODES
                STATE['workers'][data['node']] = data
                save(ROOT/'workers.json', STATE['workers'])
            elif path == '/cohort/start':
                assert set(STATE['workers']) == set(NODES)
                old = STATE['trial']
                assert not old or old['status'] in ['completed', 'failed']
                assert data['arm'] in ['store', 'peer'] and data['stratum'] in ['empty', 'one-seed']
                name = data['trial']
                assert name.replace('-', '').isalnum() and not (ROOT/'runs'/name).exists()
                trial = dict(data, nodes=NODES, status='preparing', ready={}, results={},
                             started={}, pulled={}, created_utc=time.time(), commands={})
                peers = {n: 'http://'+STATE['workers'][n]['ip']+':7071' for n in NODES}
                for n in NODES:
                    trial['commands'][n] = dict(data, peers=[u for nn,u in peers.items() if nn != n],
                                                seed=(n == NODES[0]))
                STATE['trial'] = trial
                STATE['counters'] = BodyCounter()
                threading.Thread(target=finish_trial, args=(trial,), daemon=True).start()
            elif path in ['/ready', '/started', '/pulled', '/result']:
                t = STATE['trial']
                assert t and data['trial'] == t['trial'] and data['node'] in NODES
                node = data['node']
                if path == '/ready':
                    assert node not in t['ready']
                    t['ready'][node] = data
                elif path == '/started':
                    assert 'release_mono' in t and node not in t['started']
                    t['started'][node] = time.monotonic()
                elif path == '/pulled':
                    assert node not in t['pulled']
                    t['pulled'][node] = time.monotonic()
                else:
                    assert node not in t['results']
                    t['results'][node] = data
            else:
                return self.reply(dict(error='unknown route'), 404)
        self.reply(dict(ok=True))


class Proxy(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def do_HEAD(self):
        self.forward()

    def do_GET(self):
        self.forward()

    def forward(self):
        # Workers only READ the immutable source. Reject mutation at the proxy.
        conn = http.client.HTTPConnection('127.0.0.1', 7070, timeout=180)
        count = 0
        written = 0
        transfer_error = None
        status = 502
        with LOCK:
            STATE['active_proxy'] += 1
            counter = STATE['counters']
            trial = STATE['trial']
            trial_id = trial['trial'] if trial else 'bootstrap'
        try:
            headers = {k: v for k,v in self.headers.items() if k.lower() not in ['connection', 'proxy-connection']}
            conn.request(self.command, self.path, headers=headers)
            response = conn.getresponse()
            status = response.status
            self.send_response(status)
            for k,v in response.getheaders():
                if k.lower() not in ['connection', 'transfer-encoding', 'server', 'date']:
                    self.send_header(k,v)
            self.send_header('Connection', 'close')
            self.end_headers()
            if self.command != 'HEAD':
                while b := response.read(1 << 20):
                    count += len(b)
                    self.wfile.write(b)
                    written += len(b)
        except BaseException:
            transfer_error = traceback.format_exc()
        finally:
            conn.close()
            counter.record(self.path, count, status, bool(transfer_error))
            record = dict(utc=time.time(), trial=trial_id, method=self.command,
                          path=self.path, status=status, body_bytes=count, written_body_bytes=written,
                          transfer_error=transfer_error, client=self.client_address[0])
            with LOCK:
                with (ROOT/'origin-requests.jsonl').open('a') as f:
                    f.write(json.dumps(record)+'\n')
                STATE['active_proxy'] -= 1
            self.close_connection = True


def main():
    ROOT.mkdir(exist_ok=True)
    # Dedicated disposable test identity, same public fixture credentials as CI.
    gateway = subprocess.Popen([str(ROOT/'versitygw'), '--access', 'onggitest',
        '--secret', 'onggitestsecret', '--port', '127.0.0.1:7070', 'posix', '/data'],
        stdout=open(ROOT/'gateway.log','ab'), stderr=subprocess.STDOUT)
    threading.Thread(target=ThreadingHTTPServer(('0.0.0.0',9000),Proxy).serve_forever,daemon=True).start()
    try:
        ThreadingHTTPServer(('0.0.0.0',9010),Admin).serve_forever()
    finally:
        gateway.terminate()
        gateway.wait(timeout=20)


if __name__ == '__main__':
    main()
