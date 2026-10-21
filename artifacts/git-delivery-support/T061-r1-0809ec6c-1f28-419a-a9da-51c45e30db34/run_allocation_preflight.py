from __future__ import annotations
import datetime as dt,hashlib,json,subprocess,sys,time
from pathlib import Path
sys.stdout.reconfigure(encoding='utf-8',errors='replace')
root=Path.cwd(); delivery='0809ec6c-1f28-419a-a9da-51c45e30db34'; base='b14dcd101ad7b2b4bec18c7f05509430f9ac1e1f'; support=Path(f'artifacts/git-delivery-support/T061-r1-{delivery}')
plan_ref=f'{support.as_posix()}/check-plan-canonical.json'; plan_sha='1fcd5926248a5390f96e42d45ac2c6b6b876150420fbdbad41c0675c7bdfccb3'; attempt=int(sys.argv[1]) if len(sys.argv)>1 else 2
attempt_dir=support/f'local-checks/allocation/attempt-{attempt}'; attempt_dir.mkdir(parents=True,exist_ok=False); records_dir=support/'local-checks/allocation/records'
def now(): return dt.datetime.now(dt.timezone.utc).isoformat(timespec='milliseconds').replace('+00:00','Z')
commands=[('delivery-plan',['python','-B','tools/ci/delivery_policy.py','check-plan','--plan',plan_ref,'--workflow','.github/workflows/bootstrap.yml','--sha256',plan_sha]),('spec-check',['python','-B','tools/spec_check/check.py']),('spec-check-fixtures',['python','-B','tools/spec_check/test_spec_check.py']),('git-diff-check',['git','diff','--check'])]
checks=[]
for check_id,argv in commands:
 start=now(); tick=time.monotonic(); p=subprocess.run(argv,cwd=root,capture_output=True,check=False); end=now(); output=p.stdout+p.stderr
 logname=f'{check_id}-attempt-{attempt}.log'; recordname=f'{check_id}-attempt-{attempt}.json'; logref=f'{support.as_posix()}/local-checks/allocation/records/{logname}'
 (records_dir/logname).write_bytes(output); (attempt_dir/f'{check_id}.log').write_bytes(output)
 record={'argv':argv,'check_id':check_id,'delivery_id':delivery,'ended_at_utc':end,'exit_code':p.returncode,'format_version':1,'kind':'local_check_execution','log_ref':logref,'log_sha256':hashlib.sha256(output).hexdigest(),'result':'PASS' if p.returncode==0 else 'FAIL','started_at_utc':start,'task_id':'T061','task_revision':1}
 (records_dir/recordname).write_text(json.dumps(record,ensure_ascii=False,sort_keys=True,indent=2)+'\n',encoding='utf-8'); checks.append(record)
 print(json.dumps({'check_id':check_id,'result':record['result'],'elapsed_seconds':round(time.monotonic()-tick,3),'output':output.decode('utf-8','replace')[-1000:]},ensure_ascii=False))
passed=sum(c['result']=='PASS' for c in checks); failed=len(checks)-passed; result='PASS' if failed==0 else 'FAIL'
check_data={'base_sha':base,'check_plan_ref':plan_ref,'check_plan_sha256':plan_sha,'checks':checks,'delivery_id':delivery,'failed_count':failed,'format_version':1,'kind':'allocation_check','passed_count':passed,'result':result,'task_id':'T061','task_revision':1}
checkpath=support/'allocation-check.json'; checkpath.write_text(json.dumps(check_data,ensure_ascii=False,sort_keys=True,indent=2)+'\n',encoding='utf-8')
pre={'allocation_check_ref':f'{support.as_posix()}/allocation-check.json','base_sha':base,'check_plan_ref':plan_ref,'check_plan_sha256':plan_sha,'delivery_id':delivery,'failed_count':failed,'format_version':1,'kind':'allocation_preflight','passed_count':passed,'result':result,'scope_paths':['internal/contracts/events/**','internal/contracts/schema/**','tests/contract/import_boundary_test.go','Maps-Context-Engine-4D_SPEC.md','artifacts/preflight/T061/**'],'task_id':'T061','task_revision':1}
prepath=support/'allocation-preflight.json'; prepath.write_text(json.dumps(pre,ensure_ascii=False,sort_keys=True,indent=2)+'\n',encoding='utf-8')
previous=f'{support.as_posix()}/local-checks/allocation/attempt-{attempt-1}/attempt.json' if attempt>1 else None
attemptdata={'attempt':attempt,'delivery_id':delivery,'format_version':1,'kind':'allocation_preflight_attempt','previous_attempt_ref':previous,'result':result,'summary_ref':f'{support.as_posix()}/allocation-check.json','task_id':'T061','task_revision':1}
(attempt_dir/'attempt.json').write_text(json.dumps(attemptdata,ensure_ascii=False,sort_keys=True,indent=2)+'\n',encoding='utf-8')
spec=root/'Maps-Context-Engine-4D_SPEC.md'; lines=spec.read_text(encoding='utf-8').splitlines(); ri=next(i for i,x in enumerate(lines) if x.startswith('| T061 |')); parts=lines[ri].split('|'); tokens=parts[9].split(';')
values={'allocation_check':f'{result}({passed}/4 local)','allocation_local':f'{result}({passed}/4)','allocation_check_sha256':hashlib.sha256(checkpath.read_bytes()).hexdigest(),'allocation_preflight_sha256':hashlib.sha256(prepath.read_bytes()).hexdigest()}
for i,t in enumerate(tokens):
 if '=' in t:
  k,_=t.split('=',1)
  if k in values: tokens[i]=f'{k}={values[k]}'
parts[9]=';'.join(tokens); lines[ri]='|'.join(parts)
s=support; summary_refs=[f'{s.as_posix()}/archive-view/allocation-attempt-2-check.json sha256:{hashlib.sha256((s/"archive-view/allocation-attempt-2-check.json").read_bytes()).hexdigest()}', f'{s.as_posix()}/archive-view/allocation-attempt-2-preflight.json sha256:{hashlib.sha256((s/"archive-view/allocation-attempt-2-preflight.json").read_bytes()).hexdigest()}']
for c in checks:
 stem=c['check_id']+f'-attempt-{attempt}'
 for suffix in ('.json','.log'):
  path=records_dir/(stem+suffix); summary_refs.append(f'{support.as_posix()}/local-checks/allocation/records/{path.name} sha256:{hashlib.sha256(path.read_bytes()).hexdigest()}')
old=f'Attempt1 FAIL 3/4 do S22 task IDs/bộ đếm bị quote; sửa về scalar. Attempt2 PASS 4/4 trước khi thêm exact T060 local-close refs vào SPEC nên bị invalidated; attempt {attempt} {result} {passed}/4 chạy trên allocation bytes cuối.'
entry=f'''| E677 | T061 | task_revision=1;delivery={delivery};phase=allocation;plan_sha256={plan_sha};local={result}({passed}/4);allocation_check_sha256={values['allocation_check_sha256']};result={result} | Windows 11 x64; Python 3.12; Go 1.27.2; GitHub Actions Ubuntu/Windows | Cấp T061 theo S47 P09 sau T060 effective DONE; D088 chốt shared AsyncAPI identity và import guard trước code. | {result} — {old} Frozen six-context check plan hợp lệ; chưa có product code trong allocation. | {support.as_posix()}/selection.json sha256:{hashlib.sha256((support/'selection.json').read_bytes()).hexdigest()}; {support.as_posix()}/allocation.json sha256:{hashlib.sha256((support/'allocation.json').read_bytes()).hexdigest()}; {plan_ref} sha256:{plan_sha}; {support.as_posix()}/archive-view/allocation-attempt-1-check.json sha256:{hashlib.sha256((support/'archive-view/allocation-attempt-1-check.json').read_bytes()).hexdigest()}; {support.as_posix()}/allocation-check.json sha256:{values['allocation_check_sha256']}; {support.as_posix()}/allocation-preflight.json sha256:{values['allocation_preflight_sha256']}; {'; '.join(summary_refs)} | {now()} |'''
ei=next(i for i,x in enumerate(lines) if x.startswith('| E677 |')); lines[ei]=entry; spec.write_text('\n'.join(lines)+'\n',encoding='utf-8')
print(json.dumps({'attempt':attempt,'result':result,'passed':passed,'failed':failed,'allocation_check_sha256':values['allocation_check_sha256'],'allocation_preflight_sha256':values['allocation_preflight_sha256']},ensure_ascii=False))
if failed: sys.exit(1)
