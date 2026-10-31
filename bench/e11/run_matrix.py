"""Central sequential cohort matrix; run in a detached owned dell_0 process."""
import argparse
import json
import pathlib
import time
import urllib.request

from common import aggregate, save

ROOT=pathlib.Path('/home/gspark/workspace/codex_e11_20261031')


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--stage',choices=['smoke','main'],required=True)
    args=parser.parse_args()
    origin=json.loads((ROOT/'origin-assignment.json').read_text())['ip']
    admin='http://'+origin+':9010'
    def api(path,data=None):
        req=urllib.request.Request(admin+path,data=None if data is None else json.dumps(data).encode(),headers={'Content-Type':'application/json'})
        with urllib.request.urlopen(req,timeout=30) as r:
            return json.load(r)
    before=api('/status')
    assert set(before['workers'])=={'cpu-1','cpu-2','etri-001'}
    save(ROOT/('workers-'+args.stage+'.json'),before['workers'])
    cohorts=[]
    n=1 if args.stage=='smoke' else 5
    for repetition in range(n):
        for stratum in ['empty','one-seed']:
            order=['store','peer'] if repetition%2==0 else ['peer','store']
            for arm in order:
                name=f'{args.stage}-{stratum}-{arm}-{repetition+1:02d}'
                api('/cohort/start',dict(trial=name,stratum=stratum,arm=arm,stage=args.stage,repetition=repetition+1))
                print('START '+name,flush=True)
                deadline=time.monotonic()+2000
                while True:
                    trial=api('/status')['trial']
                    assert trial['trial']==name
                    if trial['status'] in ['completed','failed'] and 'finished_utc' in trial:
                        break
                    assert time.monotonic()<deadline,'cohort deadline'
                    time.sleep(2)
                save(ROOT/'cohorts'/(name+'.json'),trial)
                assert trial['status']=='completed',json.dumps(trial.get('error'))
                cohorts.append(trial)
                print('DONE '+name+' '+json.dumps(trial['metrics']),flush=True)
    if args.stage=='main':
        summary=aggregate(cohorts)
        assert len(summary['groups'])==4
        save(ROOT/'summary.json',summary)
    save(ROOT/(args.stage+'-complete.json'),dict(status='completed',cohorts=len(cohorts),utc=time.time()))


if __name__=='__main__':
    main()
