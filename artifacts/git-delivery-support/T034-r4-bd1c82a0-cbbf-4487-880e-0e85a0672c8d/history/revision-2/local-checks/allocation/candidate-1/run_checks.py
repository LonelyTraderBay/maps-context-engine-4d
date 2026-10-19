from pathlib import Path
from datetime import datetime, timezone
import hashlib, json, os, platform, subprocess, sys, time

repo = Path.cwd()
out = repo / 'artifacts/git-delivery-support/T034-r2-0d5fd1bc-5be2-4ddc-a597-bdb1a6dd6594/local-checks/allocation/candidate-1'
base = 'e32f7daa80835ef9bca5978b636909b47bd4d4e6'
tree_expected = 'f4e9a12fd67e918108c05512ad51cc28142c73fc'
delivery = '0d5fd1bc-5be2-4ddc-a597-bdb1a6dd6594'
plan_sha = 'fe8ba36108e28eaddd13b7f033c5e15e1ae25b26f11ef4f530e20de820783267'
plan = str(repo / 'artifacts/git-delivery-support/T034-r2-0d5fd1bc-5be2-4ddc-a597-bdb1a6dd6594/check-plan.json')
commands = [
    ('bootstrap_check', ['python','-B','tools/ci/bootstrap_check.py']),
    ('test_bootstrap_check', ['python','-B','tools/ci/test_bootstrap_check.py']),
    ('delivery_policy_plan', ['python','-B','tools/ci/delivery_policy.py','check-plan','--plan',plan,'--sha256',plan_sha]),
    ('spec_check', ['python','-B','tools/spec_check/check.py']),
    ('test_spec_check', ['python','-B','tools/spec_check/test_spec_check.py']),
    ('test_delivery_policy', ['python','-B','tools/ci/test_delivery_policy.py']),
    ('test_checkpoint_digest', ['python','-B','tools/ci/test_checkpoint_digest.py']),
    ('check_gofmt', ['python','-B','tools/ci/check_gofmt.py']),
    ('go_test', ['go','test','./...']),
    ('go_vet', ['go','vet','./...']),
    ('go_build', ['go','build','./...']),
]

def utc(): return datetime.now(timezone.utc).isoformat(timespec='microseconds').replace('+00:00','Z')
def output(args): return subprocess.run(args,cwd=repo,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,encoding='utf-8',errors='replace',timeout=600)
def digest(data): return hashlib.sha256(data).hexdigest()
def git(args): return subprocess.check_output(['git',*args],cwd=repo,text=True,encoding='utf-8').strip()

if git(['rev-parse','HEAD']) != base: raise SystemExit('HEAD changed before allocation checks')
if git(['write-tree']) != tree_expected: raise SystemExit('staged tree differs from frozen candidate')
status=[x for x in git(['status','--porcelain','--untracked-files=no']).splitlines() if x]
if status != ['M  Maps-Context-Engine-4D_SPEC.md']: raise SystemExit(f'unexpected tracked working tree state: {status!r}')
start=utc(); records=[]; failed=[]
for name, argv in commands:
    if git(['rev-parse','HEAD']) != base or git(['write-tree']) != tree_expected:
        raise SystemExit(f'checkout changed during check sequence before {name}')
    begun=utc(); tick=time.monotonic()
    try:
        result=output(argv); code=result.returncode; log=result.stdout
    except subprocess.TimeoutExpired as exc:
        code=124; log=(exc.stdout or '') + '\nTIMEOUT after 600 seconds\n'
    ended=utc(); elapsed=round(time.monotonic()-tick,3)
    log_path=out/f'{name}.log'; log_path.write_text(log,encoding='utf-8',newline='\n')
    rec={'name':name,'argv':argv,'started_at_utc':begun,'completed_at_utc':ended,'duration_seconds':elapsed,'exit_code':code,'log':log_path.name,'log_sha256':digest(log_path.read_bytes())}
    records.append(rec)
    print(f'{name}: {"PASS" if code==0 else "FAIL"} ({elapsed}s)',flush=True)
    if code: failed.append(name)
completed=utc(); passed=len(records)-len(failed)
receipt={'format_version':1,'kind':'allocation_check_receipt','task_id':'T034','task_revision':2,'delivery_id':delivery,'phase':'allocation','base_sha':base,'candidate_sha':'PENDING','candidate_tree_sha':tree_expected,'check_plan_sha256':plan_sha,'result':'PASS' if not failed and len(records)==11 else 'FAIL','check_count':len(records),'passed_checks':passed,'failed_checks':len(failed),'checks':records,'checkout_binding':{'base_head_sha':base,'staged_paths':['Maps-Context-Engine-4D_SPEC.md'],'candidate_tree_from_index':True,'candidate_tree_matches_commit':False}}
(out/'allocation-receipt.json').write_text(json.dumps(receipt,ensure_ascii=False,sort_keys=True,separators=(',',':'))+'\n',encoding='utf-8',newline='\n')
summary={'format_version':1,'kind':'local_check_summary','task_id':'T034','task_revision':2,'delivery_id':delivery,'phase':'allocation','candidate_sha':'PENDING','tree_sha':tree_expected,'result':receipt['result'],'planned_checks':11,'completed_checks':len(records),'passed_checks':passed,'failed_checks':len(failed),'started_at_utc':start,'completed_at_utc':completed,'records':[r['log'] for r in records],'receipt_ref':'allocation-receipt.json','environment':{'os':platform.platform(),'python':platform.python_version(),'git':git(['--version']),'go':subprocess.check_output(['go','version'],cwd=repo,text=True).strip()}}
(out/'summary.json').write_text(json.dumps(summary,ensure_ascii=False,sort_keys=True,separators=(',',':'))+'\n',encoding='utf-8',newline='\n')
print(json.dumps({'result':receipt['result'],'passed':passed,'failed':len(failed),'candidate_tree_sha':tree_expected,'failed_checks':failed},ensure_ascii=False),flush=True)
if failed or len(records)!=11: sys.exit(1)
