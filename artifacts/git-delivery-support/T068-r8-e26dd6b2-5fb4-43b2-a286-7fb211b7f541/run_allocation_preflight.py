from pathlib import Path
import fnmatch, hashlib, json, os, subprocess, sys, time
from datetime import datetime, timezone

root = Path.cwd().resolve()
delivery = 'e26dd6b2-5fb4-43b2-a286-7fb211b7f541'
sup = f'artifacts/git-delivery-support/T068-r8-{delivery}'
out = root/sup/'local-checks/allocation/attempt-3'
records = out/'records'
records.mkdir(parents=True, exist_ok=False)


def utc(): return datetime.now(timezone.utc).isoformat(timespec='microseconds').replace('+00:00','Z')
def sha_bytes(b): return hashlib.sha256(b).hexdigest()
def write_json(p, obj):
    b=(json.dumps(obj,ensure_ascii=False,indent=2)+'\n').encode('utf-8')
    p.write_bytes(b)
    return sha_bytes(b)
def run(check_id, actual_argv, inline=None):
    start=utc(); begun=time.monotonic()
    if inline:
        try:
            inline(); code=0; raw=b'PASS\n'
        except Exception as e:
            code=1; raw=(f'FAIL: {type(e).__name__}: {e}\n').encode('utf-8')
        result='PASS' if code==0 else 'FAIL'
        stored_argv=['python',f'<inline {check_id} assertions>']
    else:
        proc=subprocess.run(actual_argv,cwd=root,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,check=False)
        code=proc.returncode; raw=proc.stdout; result='PASS' if code==0 else 'FAIL'
        stored_argv=['python',*actual_argv[1:]] if actual_argv and actual_argv[0]==sys.executable else actual_argv
    end=utc(); relbase=f'{sup}/local-checks/allocation/attempt-3/records/{check_id}'
    log_rel=relbase+'.log'; rec_rel=relbase+'.json'
    (root/log_rel).write_bytes(raw)
    rec={'argv':stored_argv,'cwd':'<repository-root>','delivery_id':delivery,'elapsed_seconds':round(time.monotonic()-begun,3),'ended_at_utc':end,'exit_code':code,'format_version':1,'kind':'local_check_execution','log_ref':log_rel,'log_sha256':sha_bytes(raw),'record_ref':rec_rel,'result':result,'started_at_utc':start,'task_id':'T068','task_revision':6,'check_id':check_id}
    recsha=write_json(root/rec_rel,rec)
    return {'check_id':check_id,'record_ref':rec_rel,'sha256':recsha,'result':result,'exit_code':code,'log_ref':log_rel,'log_sha256':sha_bytes(raw),'output':raw.decode('utf-8','replace')[:1600]}

checks=[]
plan_rel=f'{sup}/check-plan-canonical.json'
checks.append(run('delivery-plan',[sys.executable,'-B','tools/ci/delivery_policy.py','check-plan','--plan',plan_rel]))
checks.append(run('spec-check',[sys.executable,'-B','tools/spec_check/check.py']))
checks.append(run('spec-fixtures',[sys.executable,'-B','tools/spec_check/test_spec_check.py']))
checks.append(run('git-diff-check',['git','diff','--check']))

def fixture_assert():
    p=json.loads((root/'artifacts/preflight/T068/polyglot-acceptance-plan-r2.json').read_text(encoding='utf-8'))
    ids=[x['id'] for x in p['cases']]
    assert p['kind']=='polyglot_acceptance_plan'
    assert ids==[f'C{i:03d}' for i in range(55,70)]
    c=p['cohort']
    assert c['minimum_repositories']==3 and c['minimum_tasks']>=30 and c['repeats']==3 and c['randomization_seed']==68055
    assert c['modes']==['baseline','full','incremental'] and c['repositories']==['alpha','beta','gamma']
    assert c['real_repository_result']=='NOT_RUN'
    assert 'independently authored' in p['oracle'] and 'never its own oracle' in p['oracle']
    assert p['report_contract']['correctness_failure_may_not_be_offset_by_context_savings'] is True
    assert p['report_contract']['token_savings_claim_without_host_receipt']=='FORBIDDEN'
    assert p['report_contract']['token_usage_status_without_host_receipt']=='NOT_RUN'
checks.append(run('fixture-plan-json',['python','<inline fixture-plan assertions>'],fixture_assert))

def allocation_assert():
    rels=[f'{sup}/selection.json',f'{sup}/task-mode.json',f'{sup}/dependency-review.json',f'{sup}/allocation-current.json',f'{sup}/allocation-revalidation.json',f'{sup}/scope-amendment/scope-amendment-r8-windows-longpaths.json',plan_rel,'artifacts/preflight/T068/polyglot-acceptance-plan-r2.json']
    docs={r:json.loads((root/r).read_text(encoding='utf-8')) for r in rels}
    sel=docs[f'{sup}/selection.json']; mode=docs[f'{sup}/task-mode.json']; dep=docs[f'{sup}/dependency-review.json']; alloc=docs[f'{sup}/allocation-current.json']; reval=docs[f'{sup}/allocation-revalidation.json']; amendment=docs[f'{sup}/scope-amendment/scope-amendment-r8-windows-longpaths.json']; plan=docs[plan_rel]; fixture=docs['artifacts/preflight/T068/polyglot-acceptance-plan-r2.json']
    assert sel['task_id']==mode['task_id']==dep['task_id']==alloc['task_id']=='T068' and sel['task_revision']==mode['task_revision']==alloc['task_revision']==6
    assert sel['active_delivery_id']==mode['delivery_id']==dep['delivery_id']==alloc['delivery_id']==delivery
    assert alloc['base_sha']==reval['observed_base_sha']=='26c7ae5214a9192eed224df5cfe10585d2dd0c7c'
    assert reval['allocation_sha256']==sha_bytes((root/f'{sup}/allocation-current.json').read_bytes())
    assert reval['scope_amendment_ref']==f'{sup}/scope-amendment/scope-amendment-r8-windows-longpaths.json'
    assert reval['scope_amendment_sha256']==sha_bytes((root/reval['scope_amendment_ref']).read_bytes())
    assert dep['result']=='READY' and {d['task_id']:d['effective_status'] for d in dep['dependencies']}=={f'T{i:03d}':'DONE' for i in range(63,68)}
    assert set(mode['change_budget']['allowed_paths'])==set(alloc['scope_paths']) and '.gitattributes' in alloc['scope_paths']
    assert len(plan['checks'])==6 and plan['canary_ref']==f'delivery:{delivery}:payload'
    assert sha_bytes((root/plan_rel).read_bytes())=='d7a2e8067c494219cdd2c293e920802aabb657b5db495991c7bf39299164feea'
    assert fixture['task_id']=='T068' and fixture['task_revision']==2 and len(fixture['cases'])==15
    assert amendment['added_scope_paths']==['.github/workflows/bootstrap.yml',f'{sup}/**'] and amendment['evidence']['attributes_sha256']==sha_bytes((root/'.gitattributes').read_bytes())
    assert amendment['evidence']['added_rules']==[f'{sup}/** -text',f'{sup}/**/*.log whitespace=cr-at-eol']
checks.append(run('allocation-artifacts',['python','<inline allocation consistency assertions>'],allocation_assert))

def scope_assert():
    allowed=json.loads((root/f'{sup}/allocation-current.json').read_text(encoding='utf-8'))['scope_paths']
    tracked=subprocess.run(['git','diff','--name-only'],cwd=root,capture_output=True,text=True,check=True).stdout.splitlines()
    violations=[p for p in tracked if not any(fnmatch.fnmatchcase(p,a) for a in allowed)]
    assert not violations, f'tracked changed paths outside allocation: {violations}'
    status=subprocess.run(['git','status','--short','--untracked-files=all'],cwd=root,capture_output=True,text=True,check=True).stdout.splitlines()
    current=[]
    for line in status:
        if line.startswith('?? '):
            p=line[3:]
            if p.startswith(f'{sup}/') or p.startswith('artifacts/preflight/T068/'):
                current.append(p)
    violations=[p for p in current if not any(fnmatch.fnmatchcase(p,a) for a in allowed)]
    assert not violations, f'T068 untracked paths outside allocation: {violations}'
    assert tracked and current
checks.append(run('scope-check',['python','<inline allocation scope assertions>'],scope_assert))

failed=[x for x in checks if x['result']!='PASS']
summary={'base_sha':'26c7ae5214a9192eed224df5cfe10585d2dd0c7c','check_refs':[{'record_ref':x['record_ref'],'sha256':x['sha256']} for x in checks],'check_sha256':[x['sha256'] for x in checks],'delivery_id':delivery,'failed_count':len(failed),'format_version':1,'kind':'local_check_summary','passed_count':len(checks)-len(failed),'result':'FAIL' if failed else 'PASS','task_id':'T068','task_revision':6}
summary_rel=f'{sup}/local-checks/allocation/attempt-3/summary.json'; summary_sha=write_json(root/summary_rel,summary)
check_doc={'base_sha':'26c7ae5214a9192eed224df5cfe10585d2dd0c7c','check_plan_ref':plan_rel,'check_plan_sha256':'d7a2e8067c494219cdd2c293e920802aabb657b5db495991c7bf39299164feea','check_refs':[{'record_ref':x['record_ref'],'sha256':x['sha256']} for x in checks],'checked_at_utc':utc(),'delivery_id':delivery,'failed_count':len(failed),'format_version':1,'kind':'allocation_check','passed_count':len(checks)-len(failed),'result':'FAIL' if failed else 'PASS','summary_ref':summary_rel,'summary_sha256':summary_sha,'task_id':'T068','task_revision':6}
check_rel=f'{sup}/allocation-check.json'; check_sha=write_json(root/check_rel,check_doc)
alloc=json.loads((root/f'{sup}/allocation-current.json').read_text(encoding='utf-8'))
preflight={'allocation_check_ref':check_rel,'allocation_check_sha256':check_sha,'base_sha':'26c7ae5214a9192eed224df5cfe10585d2dd0c7c','check_plan_ref':plan_rel,'check_plan_sha256':'d7a2e8067c494219cdd2c293e920802aabb657b5db495991c7bf39299164feea','checked_at_utc':utc(),'delivery_id':delivery,'failed_count':len(failed),'format_version':1,'kind':'allocation_preflight','passed_count':len(checks)-len(failed),'result':'FAIL' if failed else 'PASS','scope_paths':alloc['scope_paths'],'task_id':'T068','task_revision':6}
pre_rel=f'{sup}/allocation-preflight.json'; pre_sha=write_json(root/pre_rel,preflight)
print(json.dumps({'delivery_id':delivery,'result':check_doc['result'],'passed':check_doc['passed_count'],'failed':check_doc['failed_count'],'summary_ref':summary_rel,'summary_sha256':summary_sha,'allocation_check_ref':check_rel,'allocation_check_sha256':check_sha,'preflight_ref':pre_rel,'preflight_sha256':pre_sha,'checks':[{'id':x['check_id'],'result':x['result'],'exit':x['exit_code'],'output':x['output']} for x in checks]},ensure_ascii=True,indent=2))
if failed: sys.exit(1)