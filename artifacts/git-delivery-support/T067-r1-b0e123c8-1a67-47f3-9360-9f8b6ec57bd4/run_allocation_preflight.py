from pathlib import Path
import hashlib,json,os,subprocess,sys,time
from datetime import datetime,timezone
root=Path.cwd(); delivery='b0e123c8-1a67-47f3-9360-9f8b6ec57bd4'; sup=f'artifacts/git-delivery-support/T067-r1-{delivery}'
out=root/sup/'local-checks/allocation/attempt-3'; records=out/'records'; records.mkdir(parents=True,exist_ok=False)
def utc(): return datetime.now(timezone.utc).isoformat(timespec='microseconds').replace('+00:00','Z')
def sha_bytes(b): return hashlib.sha256(b).hexdigest()
def write_json(p,obj):
 b=(json.dumps(obj,ensure_ascii=False,indent=2)+'\n').encode('utf-8'); p.write_bytes(b); return sha_bytes(b)
def run(check_id,argv,inline=None):
 start=utc(); t=time.monotonic()
 if inline:
  try: inline(); code=0; raw=b'PASS\n'
  except Exception as e: code=1; raw=(f'FAIL: {type(e).__name__}: {e}\n').encode('utf-8')
  result='PASS' if code==0 else 'FAIL'; stored_argv=['python',f'<inline {check_id} assertions>']
 else:
  proc=subprocess.run(argv,cwd=root,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,check=False)
  code=proc.returncode; raw=proc.stdout; result='PASS' if code==0 else 'FAIL'; stored_argv=argv
 end=utc(); log_rel=f'{sup}/local-checks/allocation/attempt-3/records/{check_id}.log'; rec_rel=f'{sup}/local-checks/allocation/attempt-3/records/{check_id}.json'
 (root/log_rel).write_bytes(raw)
 rec={'argv':stored_argv,'cwd':'<repository-root>','delivery_id':delivery,'elapsed_seconds':round(time.monotonic()-t,3),'ended_at_utc':end,'exit_code':code,'format_version':1,'kind':'local_check_execution','log_ref':log_rel,'log_sha256':sha_bytes(raw),'record_ref':rec_rel,'result':result,'started_at_utc':start,'task_id':'T067','task_revision':1,'check_id':check_id}
 recsha=write_json(root/rec_rel,rec)
 return {'check_id':check_id,'record_ref':rec_rel,'sha256':recsha,'result':result,'exit_code':code,'log_ref':log_rel,'log_sha256':sha_bytes(raw),'output':raw.decode('utf-8','replace')[:1200]}
checks=[]
checks.append(run('delivery-plan',['python','-B','tools/ci/delivery_policy.py','check-plan','--plan',f'{sup}/check-plan-canonical-r1.json','--workflow','.github/workflows/bootstrap.yml','--sha256','3c75f2453935c9d9eabc7c9647615d157ada5ae62f35130636b5fa918378ad80']))
checks.append(run('spec-check',['python','-B','tools/spec_check/check.py']))
checks.append(run('spec-fixtures',['python','-B','-m','unittest','tools.spec_check.test_spec_check']))
def fixture_assert():
 d=json.loads((root/'artifacts/preflight/T067/architecture-fixture-plan-r1.json').read_text(encoding='utf-8')); ids=[x['id'] for x in d['cases']]
 assert d['kind']=='architecture_fixture_plan' and len(ids)==len(set(ids))==6
 assert ids==['C068-allowed-direction','C068-forbidden-witness','C068-exemption-validity','C068-incomplete-coverage','C068-state-enum','C068-no-inference-or-remediation']
 assert d['source_mode']=='synthetic caller-supplied evidence only' and 'runtime process/network' in d['forbidden_inputs']
checks.append(run('fixture-plan-json',['python','<inline fixture-plan assertions>'],fixture_assert))
def allocation_assert():
 rels=[f'{sup}/selection.json',f'{sup}/task-mode.json',f'{sup}/dependency-review.json',f'{sup}/allocation-r1.json',f'{sup}/check-plan-canonical-r1.json',f'{sup}/scope-amendment/scope-amendment-r1.json','artifacts/preflight/T067/architecture-fixture-plan-r1.json']
 docs={r:json.loads((root/r).read_text(encoding='utf-8')) for r in rels}
 a=docs[f'{sup}/allocation-r1.json']; sel=docs[f'{sup}/selection.json']; mode=docs[f'{sup}/task-mode.json']; dep=docs[f'{sup}/dependency-review.json']; plan=docs[f'{sup}/check-plan-canonical-r1.json']; scope=docs[f'{sup}/scope-amendment/scope-amendment-r1.json']
 assert a['task_id']==sel['task_id']==mode['task_id']==dep['task_id']=='T067' and a['delivery_id']==delivery
 assert a['base_sha']=='bf8e7ac247fbfbe092726e408a3b32997bc82f97' and a['dependencies']=={'T056':'DONE','T058':'DONE','T062':'DONE'}
 assert dep['result']=='READY' and dep['sequence']['effective_status']=='DONE' and dep['sequence']['closing_sha']=='bf8e7ac247fbfbe092726e408a3b32997bc82f97'
 assert plan['canary_ref']==f'delivery:{delivery}:payload' and len(plan['checks'])==6
 assert 'tests/contract/import_boundary_test.go' in scope['added_scope_paths'] and scope['evidence']['source_sha256']==hashlib.sha256((root/'tests/contract/import_boundary_test.go').read_bytes()).hexdigest()
 assert hashlib.sha256((root/f'{sup}/check-plan-canonical-r1.json').read_bytes()).hexdigest()=='3c75f2453935c9d9eabc7c9647615d157ada5ae62f35130636b5fa918378ad80'
 assert hashlib.sha256((root/'artifacts/preflight/T067/architecture-fixture-plan-r1.json').read_bytes()).hexdigest()==a['fixture_plan_sha256']
 for x in ['archive-publication.json','archive-manifest.json','archive-seal.json','live-verification.json','closing-C4-run-pair-summary.json','closing-C4-main-sourcegraph-summary.json']:
  assert (root/f'{sup}/archive-view/sequence-predecessor/T066/{x}').is_file()
 assert not (root/'artifacts/preflight/T067').joinpath('unapproved').exists()
checks.append(run('allocation-artifacts',['python','<inline allocation consistency assertions>'],allocation_assert))
checks.append(run('git-diff-check',['git','diff','--check']))
failed=[x for x in checks if x['result']!='PASS']
summary={'base_sha':'bf8e7ac247fbfbe092726e408a3b32997bc82f97','check_refs':[{'record_ref':x['record_ref'],'sha256':x['sha256']} for x in checks],
 'check_sha256':[x['sha256'] for x in checks],'delivery_id':delivery,'failed_count':len(failed),'format_version':1,'kind':'local_check_summary','passed_count':len(checks)-len(failed),
 'result':'FAIL' if failed else 'PASS','task_id':'T067','task_revision':1}
summary_rel=f'{sup}/local-checks/allocation/attempt-3/summary.json'; summarysha=write_json(root/summary_rel,summary)
check_doc={'base_sha':'bf8e7ac247fbfbe092726e408a3b32997bc82f97','check_plan_ref':f'{sup}/check-plan-canonical-r1.json','check_plan_sha256':'3c75f2453935c9d9eabc7c9647615d157ada5ae62f35130636b5fa918378ad80',
 'check_refs':[{'record_ref':x['record_ref'],'sha256':x['sha256']} for x in checks],'checked_at_utc':utc(),'delivery_id':delivery,'failed_count':len(failed),'format_version':1,'kind':'allocation_check','passed_count':len(checks)-len(failed),'result':'FAIL' if failed else 'PASS','summary_ref':summary_rel,'summary_sha256':summarysha,'task_id':'T067','task_revision':1}
check_rel=f'{sup}/allocation-check-r2.json'; checksha=write_json(root/check_rel,check_doc)
preflight={'allocation_check_ref':check_rel,'allocation_check_sha256':checksha,'base_sha':'bf8e7ac247fbfbe092726e408a3b32997bc82f97','check_plan_ref':f'{sup}/check-plan-canonical-r1.json','check_plan_sha256':'3c75f2453935c9d9eabc7c9647615d157ada5ae62f35130636b5fa918378ad80','checked_at_utc':utc(),'delivery_id':delivery,'failed_count':len(failed),'format_version':1,'kind':'allocation_preflight','passed_count':len(checks)-len(failed),'result':'FAIL' if failed else 'PASS','scope_paths':json.loads((root/f'{sup}/allocation-r1.json').read_text(encoding='utf-8'))['scope_paths'],'task_id':'T067','task_revision':1}
pre_rel=f'{sup}/allocation-preflight-r2.json'; presha=write_json(root/pre_rel,preflight)
print(json.dumps({'delivery_id':delivery,'result':check_doc['result'],'passed':check_doc['passed_count'],'failed':check_doc['failed_count'],'summary_ref':summary_rel,'summary_sha256':summarysha,'allocation_check_ref':check_rel,'allocation_check_sha256':checksha,'preflight_ref':pre_rel,'preflight_sha256':presha,'checks':[{'id':x['check_id'],'result':x['result'],'exit':x['exit_code'],'output':x['output']} for x in checks]},ensure_ascii=True))
if failed: sys.exit(1)
