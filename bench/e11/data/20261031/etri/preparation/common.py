"""Payload accounting and fail-closed aggregation; Python stdlib only."""
import copy
import hashlib
import json
import pathlib
import statistics
import threading


class BodyCounter:
    def __init__(self):
        self.lock = threading.Lock()
        self.data = dict(successful_body_bytes=0, pack_body_bytes=0,
                         metadata_body_bytes=0, error_body_bytes=0,
                         requests=0, by_status={})

    def record(self, path, count, status):
        if count < 0:
            raise ValueError('negative response body count')
        with self.lock:
            d = self.data
            d['requests'] += 1
            key = str(status)
            d['by_status'][key] = d['by_status'].get(key, 0) + 1
            if 200 <= status < 300:
                d['successful_body_bytes'] += count
                kind = 'pack_body_bytes' if '/packs/' in path else 'metadata_body_bytes'
                d[kind] += count
            else:
                d['error_body_bytes'] += count

    def snapshot(self):
        with self.lock:
            return copy.deepcopy(self.data)


def save(path, obj):
    path = pathlib.Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(path.suffix + '.tmp')
    temp.write_text(json.dumps(obj, indent=2) + '\n', encoding='utf-8')
    temp.replace(path)


def file_sha(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(4 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def validate_cohort(c):
    nodes = set(c['nodes'])
    if (len(nodes) < 3 or len(nodes) != len(c['nodes'])
            or c['status'] != 'completed'
            or any(set(c[key]) != nodes for key in ['results', 'started', 'pulled'])):
        raise ValueError('cohort missing, duplicate, or failed nodes')
    if any(c['pulled'][n] < c['started'][n] or c['started'][n] < c['release_mono'] for n in nodes):
        raise ValueError('cohort timestamp order invalid')
    for r in c['results'].values():
        if (r['status'] != 'completed' or r['returncode'] != 0
                or not r['hashes_match'] or r['pull_s'] <= 0):
            raise ValueError('node process/hash/time quality gate failed')
        if any(r['stats'][key] < 0 for key in ['read_bytes', 'peer_bytes', 'cache_bytes']):
            raise ValueError('negative source payload')
    if len({r['manifest'] for r in c['results'].values()}) != 1:
        raise ValueError('nodes materialized different manifests')
    return dict(cohort_completion_s=max(c['pulled'].values()) - c['release_mono'],
                received_start_skew_s=max(c['started'].values()) - min(c['started'].values()),
                store_read_bytes=sum(r['stats']['read_bytes'] for r in c['results'].values()),
                peer_bytes=sum(r['stats']['peer_bytes'] for r in c['results'].values()),
                cache_bytes=sum(r['stats']['cache_bytes'] for r in c['results'].values()),
                **c['origin'])


def distribution(values):
    q1, _, q3 = statistics.quantiles(values, method='inclusive')
    return dict(n=len(values), median=statistics.median(values), q1=q1, q3=q3, iqr=q3-q1,
                samples=values)


def aggregate(cohorts):
    groups = {}
    seen = set()
    smokes = []
    for c in cohorts:
        if c['trial'] in seen:
            raise ValueError('duplicate cohort identifier')
        seen.add(c['trial'])
        metrics = validate_cohort(c)
        if c['stage'] == 'smoke':
            smokes.append(c['trial'])
            continue
        if c['stage'] != 'main':
            raise ValueError('unknown trial stage')
        key = (c['stratum'], c['arm'], tuple(c['nodes']))
        groups.setdefault(key, []).append((c, metrics))
    out = []
    for (stratum, arm, nodes), rows in sorted(groups.items()):
        if len(rows) < 5:
            raise ValueError('incomplete n>=5 group')
        item = dict(stratum=stratum, arm=arm, nodes=nodes,
                    trials=[c['trial'] for c, _ in rows])
        for metric in ['cohort_completion_s', 'received_start_skew_s', 'store_read_bytes',
                       'peer_bytes', 'cache_bytes', 'successful_body_bytes',
                       'pack_body_bytes', 'metadata_body_bytes', 'requests']:
            item[metric] = distribution([m[metric] for _, m in rows])
        item['per_node_pull_s'] = {n: distribution([c['results'][n]['pull_s'] for c, _ in rows]) for n in nodes}
        out.append(item)
    return dict(schema='onggi.e11.summary.v1', groups=out, excluded_smoke=smokes,
                quartiles='statistics.quantiles inclusive', failed_main_excluded=0)
