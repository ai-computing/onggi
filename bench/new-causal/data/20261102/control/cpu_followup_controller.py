"""Observe existing CPU timing Job, then run already-frozen IO/publication cohorts.

No retries: a failed/invalid/unknown attempt stops dependent execution.
Every mutation uses the existing hub transport and UID-scoped Job helpers.
"""
import argparse
import json
from pathlib import Path
import subprocess
import sys
import time
import traceback

from control import rpc, ROOT, REMOTE

PLAN = [
    ('timing', 'main', 'cdx-resident-cpu-timing-main', 5400),
    ('io', 'smoke', 'cdx-resident-cpu-io-smoke', 2400),
    ('io', 'main', 'cdx-resident-cpu-io-main', 5400),
    ('publish', 'smoke', 'cdx-resident-cpu-publish-smoke', 5400),
    ('publish', 'main', 'cdx-resident-cpu-publish-main', 10800),
]


def invoke(script, *args):
    result = subprocess.run([sys.executable, str(ROOT / script), *map(str, args)],
                            cwd=ROOT, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(script + ': ' + result.stdout[-1200:] + result.stderr[-1200:])
    return result.stdout


def inspect(name, uid):
    invoke('inspect_job.py', name)
    row = json.loads((ROOT / (name + '-latest-status.json')).read_bytes())
    assert row['job']['metadata']['uid'] == uid
    return row


def cohort(kind, stage):
    code = f'''import pathlib,json
p=pathlib.Path({REMOTE!r})/'cpu-model-a01'/{(kind+'-'+stage)!r}/'cohort.json'
r=json.loads(p.read_text())
print(json.dumps(dict(status=r['status'],valid=r['valid'],trials=len(r['trials']),
 stage=r['stage'],arms=r['arms'],frozen_sha256=r['frozen_sha256'],
 failures=[dict(arm=t['arm'],error=t.get('error')) for t in r['trials'] if not t['valid']],
 summaries={{arm:dict(valid=s['valid'],n=len(s['samples'])) for arm,s in r.get('summary',{{}}).items()}})))
'''
    return rpc('rebellion', code, 'controller-cpu-' + kind + '-' + stage)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--dry-run', action='store_true')
    args = parser.parse_args()
    first = json.loads((ROOT / (PLAN[0][2] + '-create.json')).read_bytes())
    assert first['job_uid'] == '9686d329-b413-45fd-bbcf-5e8755e69362'
    if args.dry_run:
        assert len({p[2] for p in PLAN}) == len(PLAN)
        for _, _, name, _ in PLAN[1:]:
            assert not (ROOT / (name + '-create.json')).exists(), name
        print(json.dumps(dict(plan=PLAN, first_job_uid=first['job_uid'], mutations=False)))
        return
    state_path = ROOT / 'cpu-followup-controller-state.json'
    assert not state_path.exists(), 'No automatic controller restart/adoption'
    state = dict(status='running', started_utc=time.time(), completed=[], active=None)

    def save():
        state_path.write_text(json.dumps(state, indent=2) + '\n', encoding='utf-8')

    save()
    try:
        for index, (kind, stage, name, deadline) in enumerate(PLAN):
            if index:
                invoke('create_cpu_model_job.py', name, 'cpu_run_matrix.py', kind, stage,
                       '--deadline', deadline)
            created = json.loads((ROOT / (name + '-create.json')).read_bytes())
            uid = created['job_uid']
            state['active'] = dict(kind=kind, stage=stage, name=name, uid=uid)
            save()
            until = time.monotonic() + deadline + 120
            while True:
                row = inspect(name, uid)
                conditions = row['job'].get('status', {}).get('conditions', [])
                terminal = any(c['type'] in ('Complete', 'Failed') and c['status'] == 'True' for c in conditions)
                if terminal:
                    assert all(p['status']['phase'] in ('Succeeded', 'Failed') for p in row['pods'])
                    succeeded = any(c['type'] == 'Complete' and c['status'] == 'True' for c in conditions)
                    if succeeded:
                        proof = cohort(kind, stage)
                        state['active']['cohort'] = proof
                        valid = proof['status'] == 'succeeded' and proof['valid']
                        expected = len(proof['arms']) * (5 if stage == 'main' else 1)
                        valid = valid and proof['trials'] == expected and all(
                            s['valid'] and s['n'] == (5 if stage == 'main' else 1)
                            for s in proof['summaries'].values())
                    else:
                        valid = False
                    save()
                    invoke('cleanup_job.py', name, uid)
                    assert succeeded and valid, 'Failed/invalid CPU cohort retained; dependent sequence stopped'
                    state['completed'].append(state['active'])
                    state['active'] = None
                    save()
                    break
                assert time.monotonic() < until, 'Unknown completion; no deletion or successor launch'
                time.sleep(30)
        state['status'] = 'completed'
    except BaseException:
        state.update(status='failed', error=traceback.format_exc())
        raise
    finally:
        state['updated_utc'] = time.time()
        save()


if __name__ == '__main__':
    main()
