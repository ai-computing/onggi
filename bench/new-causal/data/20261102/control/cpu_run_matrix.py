"""One bounded CPU cohort; use fixed conditions and preserve every failure."""
import json,pathlib,sys,time
ROOT=pathlib.Path('/home/gspark/workspace/codex_resident_delivery_20261102')
sys.path.insert(0,str(ROOT/'cpu-causal'))
import causal as h
kind,stage=sys.argv[1:3];assert kind in ('timing','io','publish') and stage in ('smoke','main')
r=ROOT/'cpu-model-a01';assert json.loads((r/'preparation.json').read_text())['status']=='completed'
assert h.sha(ROOT/'cpu-causal/causal.py')=='1546801671fc19d3871487b2d2d4f0f8e445bae82cbfd25aabf9e87351554f5a'
assert h.sha(ROOT/'bin/onggi')=='e6b8ee52a69c7cf9fe970cc1df08e93eab9c175c061baccb078725e549a15ae5'
result=h.run_matrix(r/(kind+'-frozen.json'),r/(kind+'-'+stage),'publish' if kind=='publish' else 'deploy',stage,r/(kind+'-smoke/cohort.json') if stage=='main' else None,trace=kind=='io')
print(json.dumps(dict(status=result['status'],valid=result['valid'],stage=stage,kind=kind,trials=len(result['trials']),error=result.get('error'))))
sys.exit(0 if result['valid'] and result['status']=='succeeded' else 1)
