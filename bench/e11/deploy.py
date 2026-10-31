"""Run on dell_0 via hub/orca.py. Create only isolated CPU-only bounded Jobs."""
import argparse
import json
import pathlib
import subprocess
import time
import urllib.request

from common import save

ROOT=pathlib.Path('/home/gspark/workspace/codex_e11_20261031')
NS='codex-e11-20261031'
ORIGIN='cdx-e11-origin-03'
SCRIPTS='cdx-e11-scripts-03'
NODES=['cpu-1','cpu-2','etri-001']
IMAGE='python:3.11-slim@sha256:e88e9763f943ec1834f992a4b51e0f24500486803e8bc534e5767af9ea65f6ce'
K=['/usr/bin/kubectl','--kubeconfig=/home/gspark/.kube/config','--context=kubernetes-admin@kubernetes']


def create(obj):
    raw=subprocess.check_output(K+['create','-f','-','-o','json'],input=json.dumps(obj).encode())
    result=json.loads(raw)
    save(ROOT/'control'/(obj['metadata']['name']+'-created.json'),result)
    return result


def pod_for(job):
    rows=json.loads(subprocess.check_output(K+['-n',NS,'get','pods','-l','job-name='+job,'-o','json']))['items']
    return rows[0] if len(rows)==1 else None


def template(name,node,worker,source,origin=None):
    common=dict(name='scripts',configMap=dict(name=SCRIPTS))
    sysvol=dict(name='host-sys',hostPath=dict(path='/sys',type='Directory'))
    mounts=[dict(name='scripts',mountPath='/scripts',readOnly=True),dict(name='host-sys',mountPath='/host-sys',readOnly=True)]
    env=[]
    volumes=[common,sysvol]
    if worker:
        volumes.append(dict(name='work',emptyDir=dict(sizeLimit='60Gi')))
        mounts.append(dict(name='work',mountPath='/work'))
        env=[dict(name='E11_ADMIN',value='http://'+origin+':9010'),
             dict(name='E11_STORE',value='s3://e11?endpoint='+origin+':9000&insecure=1&region=us-east-1')]
        for key,path in [('NODE_NAME','spec.nodeName'),('POD_IP','status.podIP'),('POD_UID','metadata.uid')]:
            env.append(dict(name=key,valueFrom=dict(fieldRef=dict(fieldPath=path))))
        limits=dict(cpu='2',memory='4Gi',**{'ephemeral-storage':'60Gi'})
        requests=dict(cpu='2',memory='4Gi',**{'ephemeral-storage':'40Gi'})
    else:
        volumes.extend([dict(name='work',hostPath=dict(path=str(ROOT/'attempt03'),type='Directory')),
                        dict(name='store',hostPath=dict(path='/home/gspark/workspace/codex_ccgrid_npu_20261030/store/e6-posix',type='Directory')),
                        dict(name='data',emptyDir={})])
        mounts.extend([dict(name='work',mountPath='/e11'),dict(name='data',mountPath='/data'),
                       dict(name='store',mountPath='/data/e11',readOnly=True)])
        limits=dict(cpu='2',memory='2Gi')
        requests=dict(cpu='1',memory='1Gi')
    container=dict(name='main',image=IMAGE,imagePullPolicy='IfNotPresent',env=env,
        command=['python3','-u','/scripts/'+('worker.py' if worker else 'origin.py')],
        volumeMounts=mounts,resources=dict(limits=limits,requests=requests),
        securityContext=dict(allowPrivilegeEscalation=False,capabilities=dict(drop=['ALL'])))
    spec=dict(restartPolicy='Never',automountServiceAccountToken=False,
              nodeSelector={'kubernetes.io/hostname':node},containers=[container],volumes=volumes,
              securityContext=dict(runAsUser=1000 if worker else 1002,
                                   runAsGroup=1000 if worker else 1002,fsGroup=1000),terminationGracePeriodSeconds=30)
    # No tolerations, host networking, privileged container, or accelerator resources.
    assert not any('/' in key for key in limits)
    return dict(apiVersion='batch/v1',kind='Job',metadata=dict(name=name,namespace=NS,
                labels={'app':'cdx-e11'},annotations={'e11.source-sha':source}),
                spec=dict(backoffLimit=0,activeDeadlineSeconds=14400,ttlSecondsAfterFinished=600,
                          template=dict(metadata=dict(labels={'app':'cdx-e11'}),spec=spec)))


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--source',required=True)
    parser.add_argument('--workers',action='store_true')
    args=parser.parse_args()
    ROOT.mkdir(parents=True,exist_ok=True)
    if not args.workers:
        if (ROOT/'namespace.json').exists():
            receipt=json.loads((ROOT/'namespace.json').read_text())
            existing=json.loads(subprocess.check_output(K+['get','namespace',NS,'-o','json']))
            assert existing['metadata']['uid']==receipt['uid']
        else:
            namespace=create(dict(apiVersion='v1',kind='Namespace',metadata=dict(name=NS,labels={'app':'cdx-e11'})))
            save(ROOT/'namespace.json',dict(name=NS,uid=namespace['metadata']['uid']))
        create(dict(apiVersion='v1',kind='ConfigMap',metadata=dict(name=SCRIPTS,namespace=NS),
                    data={name:(ROOT/name).read_text() for name in ['common.py','origin.py','worker.py']}))
        create(template(ORIGIN,'etri-001',False,args.source))
        print(json.dumps(dict(status='origin-created')))
        return
    p=pod_for(ORIGIN)
    assert p and p['status'].get('podIP') and p['status']['phase']=='Running'
    origin=p['status']['podIP']
    with urllib.request.urlopen('http://'+origin+':9010/status',timeout=10) as r:
        assert 'workers' in json.load(r)
    save(ROOT/'origin-assignment.json',dict(ip=origin,uid=p['metadata']['uid'],node=p['spec']['nodeName']))
    for i,node in enumerate(NODES):
        create(template('cdx-e11-worker-03-'+str(i),node,True,args.source,origin))
    print(json.dumps(dict(status='workers-created',origin=origin)))


if __name__=='__main__':
    main()
