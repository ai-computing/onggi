"""After n5 matrix only: stop, capture terminal APIs, UID-delete owned Jobs."""
import json
import pathlib
import subprocess
import time
import urllib.request

ROOT=pathlib.Path('/home/gspark/workspace/codex_e11_20261031')
NS='codex-e11-20261031'
K=['/usr/bin/kubectl','--kubeconfig=/home/gspark/.kube/config','--context=kubernetes-admin@kubernetes']


def get(kind):
    return json.loads(subprocess.check_output(K+['-n',NS,'get',kind,'-o','json']))['items']


def write(name,obj):
    (ROOT/name).write_text(json.dumps(obj,indent=2)+'\n')


def main():
    assert json.loads((ROOT/'main-complete.json').read_text())['cohorts']==20
    summary=json.loads((ROOT/'summary.json').read_text())
    assert len(summary['groups'])==4 and all(g['cohort_completion_s']['n']==5 for g in summary['groups'])
    receipt=json.loads((ROOT/'namespace.json').read_text())
    namespace=json.loads(subprocess.check_output(K+['get','namespace',NS,'-o','json']))
    assert namespace['metadata']['uid']==receipt['uid']
    expected={name:json.loads((ROOT/'control'/(name+'-created.json')).read_text())['metadata']['uid']
              for name in ['cdx-e11-origin-03']+['cdx-e11-worker-03-'+str(i) for i in range(3)]}
    jobs=get('jobs')
    assert {j['metadata']['name']:j['metadata']['uid'] for j in jobs}==expected
    origin=json.loads((ROOT/'origin-assignment.json').read_text())['ip']
    def api(path):
        req=urllib.request.Request('http://'+origin+':9010'+path,
            data=None if path=='/status' else b'{}',headers={'Content-Type':'application/json'})
        with urllib.request.urlopen(req,timeout=30) as r:
            return json.load(r)
    write('origin-last-status.json',api('/status'))
    api('/stop')
    for name in expected:
        if 'worker' in name:
            subprocess.run(K+['-n',NS,'wait','--for=condition=complete','job/'+name,'--timeout=90s'],capture_output=True,check=True)
    api('/shutdown')
    subprocess.run(K+['-n',NS,'wait','--for=condition=complete','job/cdx-e11-origin-03','--timeout=90s'],capture_output=True,check=True)
    jobs=get('jobs');pods=get('pods')
    assert len(jobs)==4 and len(pods)==4 and all(p['status']['phase']=='Succeeded' for p in pods)
    write('final-terminal-api.json',dict(namespace=namespace,jobs=jobs,pods=pods,utc=time.time()))
    for p in pods:
        name=p['metadata']['name']
        assert name.startswith('cdx-e11-')
        (ROOT/(name+'.log')).write_bytes(subprocess.check_output(K+['-n',NS,'logs',name]))
    for job in jobs:
        name=job['metadata']['name'];uid=job['metadata']['uid']
        assert expected[name]==uid
        opts=dict(apiVersion='v1',kind='DeleteOptions',propagationPolicy='Foreground',preconditions=dict(uid=uid))
        subprocess.run(K+['delete','--raw','/apis/batch/v1/namespaces/'+NS+'/jobs/'+name,'-f','-'],
                       input=json.dumps(opts).encode(),capture_output=True,check=True)
    until=time.monotonic()+90
    while get('jobs') or get('pods'):
        assert time.monotonic()<until
        time.sleep(.5)
    write('final-cleanup.json',dict(namespace_uid=receipt['uid'],jobs_absent=True,pods_absent=True,
        job_uids=[j['metadata']['uid'] for j in jobs],pod_uids=[p['metadata']['uid'] for p in pods],
        uid_guarded=True,utc=time.time()))
    nodes=json.loads(subprocess.check_output(K+['get','nodes','-o','json']))['items']
    write('final-nodes.json',[dict(name=n['metadata']['name'],addresses=n['status']['addresses'],
        conditions=n['status']['conditions'],nodeInfo=n['status']['nodeInfo']) for n in nodes])
    print(json.dumps(dict(cleanup_completed=True,jobs=4,pods=4,accelerators_requested=0)))


if __name__=='__main__':
    main()
