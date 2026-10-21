from __future__ import annotations
import hashlib,json,os,shutil,subprocess,sys,uuid
from datetime import datetime,timedelta,timezone
from pathlib import Path
ROOT=Path.cwd().resolve(); DELIVERY='784ed9da-3a4e-4198-909f-d96b1efe6865'; TASK='T062'; REV=1
SUPPORT=Path('artifacts/git-delivery-support/T062-r1-'+DELIVERY); OPREF=SUPPORT/'archive-view/operations/allocation-push-verification'
EXPECTED_OWNER='LonelyTraderBay'; EXPECTED_NAME='maps-context-engine-4d'; EXPECTED_ID=1421031011

def now(): return datetime.now(timezone.utc).isoformat(timespec='microseconds').replace('+00:00','Z')
def canonical(v): return json.dumps(v,ensure_ascii=False,sort_keys=True,separators=(',',':')).encode('utf-8')
def write(path,data):
 path.parent.mkdir(parents=True,exist_ok=True)
 if path.exists():
  if path.read_bytes()!=data: raise SystemExit(f'preserving conflicting output: {path}')
 else:
  with path.open('xb') as f: f.write(data); f.flush(); os.fsync(f.fileno())
def run(args,check=True): return subprocess.run(args,cwd=ROOT,check=check,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
def out(args): return run(args).stdout.decode('utf-8','strict').strip()
def ghjson(args):
 started=now(); raw=run([shutil.which('gh'),'api',*args]).stdout; completed=now(); return raw,json.loads(raw),started,completed
head=out(['git','rev-parse','HEAD']); parent=out(['git','rev-parse','HEAD^'])
if head!='d6f3c306875e788e333c2035c5006bbbb213b1f3' or parent!='b4422e5dccd46523115ba4bd299332007282c227': raise SystemExit('candidate or parent changed')
remote_url=out(['git','config','--local','--get','remote.origin.url'])
remote_config=out(['git','config','--local','--get-regexp',r'^remote\.origin\.(url|fetch)$'])+'\n'
if remote_url!='https://github.com/LonelyTraderBay/maps-context-engine-4d.git': raise SystemExit('remote config does not match approved destination')
remote_digest=hashlib.sha256(remote_config.encode()).hexdigest()
repo_raw,repo,repo_start,repo_done=ghjson([f'repos/{EXPECTED_OWNER}/{EXPECTED_NAME}'])
ref_raw,ref,ref_start,ref_done=ghjson([f'repos/{EXPECTED_OWNER}/{EXPECTED_NAME}/git/ref/heads/main'])
if repo.get('id')!=EXPECTED_ID or repo.get('full_name')!=f'{EXPECTED_OWNER}/{EXPECTED_NAME}' or repo.get('visibility')!='public': raise SystemExit('fresh repo identity/visibility mismatch')
if ref.get('object',{}).get('sha')!=head: raise SystemExit('fresh main is not already at this payload candidate; do not push')
operation_id=str(uuid.uuid4()); lifecycle_id=str(uuid.uuid4()); command_id=str(uuid.uuid4()); run_id=str(uuid.uuid4())
repo_projection={'host':'github.com','name':EXPECTED_NAME,'owner':EXPECTED_OWNER,'publication_authority_ref':f'{SUPPORT.as_posix()}/authority/task-start.txt','ref':'refs/heads/main','remote_name':'origin','remote_url_sanitized':remote_url,'repository_id':EXPECTED_ID,'visibility':'public'}
pre_query_id=str(uuid.uuid4()); pre_ref=f'{OPREF.as_posix()}/raw/preflight-query-projection.json'
write(ROOT/pre_ref,canonical({'kind':'destination_query_projection','phase':'preflight','query_id':pre_query_id,'lifecycle_id':lifecycle_id,'operation_id':operation_id,'repository':repo,'ref':ref,'repo_query_started_at_utc':repo_start,'repo_query_completed_at_utc':repo_done,'ref_query_started_at_utc':ref_start,'ref_query_completed_at_utc':ref_done,'remote_config_digest':remote_digest}))
write(ROOT/(OPREF/'raw/preflight-repository.json'),repo_raw)
write(ROOT/(OPREF/'raw/preflight-ref.json'),ref_raw)
preflight_completed=ref_done
pre={'candidate_sha':head,'completed_at_utc':preflight_completed,'lifecycle_id':lifecycle_id,'observed_remote_sha':head,'query_id':pre_query_id,'raw_api_ref':pre_ref,'remote_config_digest':remote_digest,'repository':repo_projection,'retry_index':0,'started_at_utc':repo_start}
argv=['push','--porcelain','origin',f'{head}:refs/heads/main']
push_started=now()
proc=run(['git',*argv],check=False)
push_completed=now(); push_output=proc.stdout+proc.stderr
write(ROOT/(OPREF/'push-output.txt'),push_output)
post_query_id=str(uuid.uuid4())
post_repo_raw,post_repo,post_repo_start,post_repo_done=ghjson([f'repos/{EXPECTED_OWNER}/{EXPECTED_NAME}'])
post_ref_raw,post_ref,post_ref_start,post_ref_done=ghjson([f'repos/{EXPECTED_OWNER}/{EXPECTED_NAME}/git/ref/heads/main'])
post_sha=post_ref.get('object',{}).get('sha')
post_ref_path=f'{OPREF.as_posix()}/raw/postflight-query-projection.json'
write(ROOT/post_ref_path,canonical({'kind':'destination_query_projection','phase':'postflight','query_id':post_query_id,'lifecycle_id':lifecycle_id,'operation_id':operation_id,'repository':post_repo,'ref':post_ref,'repo_query_started_at_utc':post_repo_start,'repo_query_completed_at_utc':post_repo_done,'ref_query_started_at_utc':post_ref_start,'ref_query_completed_at_utc':post_ref_done,'remote_config_digest':remote_digest}))
write(ROOT/(OPREF/'raw/postflight-repository.json'),post_repo_raw)
write(ROOT/(OPREF/'raw/postflight-ref.json'),post_ref_raw)
if proc.returncode!=0 or post_sha!=head: raise SystemExit(f'push/postflight did not establish candidate: exit={proc.returncode} remote={post_sha}')
post={'candidate_sha':head,'completed_at_utc':post_ref_done,'lifecycle_id':lifecycle_id,'observed_remote_sha':post_sha,'query_id':post_query_id,'raw_api_ref':post_ref_path,'remote_config_digest':remote_digest,'repository':repo_projection,'retry_index':0,'started_at_utc':push_completed}
obs={'candidate_sha':head,'format_version':1,'identity_verified':True,'local_remote_config_digest':remote_digest,'observed_at_utc':preflight_completed,
 'operation_evidence_ref':f'{OPREF.as_posix()}/push-operation.json','push_operation_id':operation_id,'push_started_at_utc':push_started,'repository':repo_projection,'visibility_verified':True}
write(ROOT/(OPREF/'destination-observation.json'),canonical(obs))
source={'argv':['git',*argv],'credential_material_recorded':False,'credential_provider':'git credential.helper=manager','credential_provider_version':'2.5.0','executable':'git','format_version':1,'git_version':out(['git','--version']),'kind':'push_command_source','working_directory_ref':'.'}
write(ROOT/(OPREF/'push-command-source.json'),canonical(source))
write(ROOT/(OPREF/'remote-config.txt'),remote_config.encode())
write(ROOT/(OPREF/'cleanup-plan.json'),canonical({'decision':'NOT_REQUIRED','format_version':1,'kind':'cleanup_plan','operation_id':operation_id,'reason':'Git push produced no owned temporary source or disposable artifacts.'}))
write(ROOT/(OPREF/'retention.json'),canonical({'authority_ref':f'{SUPPORT.as_posix()}/authority/task-start.txt','delivery_id':DELIVERY,'format_version':1,'kind':'retention_record','preserve_until':'task-closure-and-archive-readback'}))
command_record_ref=f'{OPREF.as_posix()}/command-lifecycle.json'; push_output_ref=f'{OPREF.as_posix()}/push-output.txt'
now_dt=datetime.now(timezone.utc); deadline=(now_dt+timedelta(minutes=10)).isoformat(timespec='microseconds').replace('+00:00','Z')
command_lifecycle={'assignment_ref':f'{SUPPORT.as_posix()}/authority/task-start.txt','command_state':'SUCCEEDED','contract':{
 'argv':argv,'baseline_ref':f'git:{head}','budget_refs':[],'cache_roots':[],'child_processes':[],'cleanup_permission_ref':None,
 'cleanup_plan_ref':f'{OPREF.as_posix()}/cleanup-plan.json','command_id':command_id,'command_source_ref':f'{OPREF.as_posix()}/push-command-source.json',
 'contracts':['PUSH-FRESH-01','GIT-05','GIT-06','COMMAND-LIFECYCLE-01'],'cwd':'.','deadline_at_utc':deadline,'download_roots':[],
 'executable':'git','expected_writes':[f'Không thay refs/heads/main; xác nhận main đang ở candidate đã push {head}.'],
 'fingerprints':{'candidate_commit_diff':hashlib.sha256(run(['git','diff',f'{parent}..{head}','--binary']).stdout).hexdigest(),'remote_config':remote_digest},
 'input_refs':[f'{SUPPORT.as_posix()}/check-plan-canonical.json',f'{SUPPORT.as_posix()}/authority/task-start.txt',f'git:{head}'],
 'log_roots':[push_output_ref],'monitor_interval_ms':1000,'network_scope':['github.com','api.github.com'],'output_root':OPREF.as_posix(),
 'owner':'Codex','permission_ref':f'{SUPPORT.as_posix()}/authority/task-start.txt','retention_ref':f'{OPREF.as_posix()}/retention.json',
 'run_id':run_id,'side_effect_class':'external_service','storage_mode':'bounded_small','task_mode':'implement','temporary_roots':[],
 'tool_versions':{'gh':out([shutil.which('gh'),'--version']).splitlines()[0],'git':out(['git','--version']),'git_credential_manager':'2.5.0'},'volumes':[]},
 'delivery_id':DELIVERY,'format_version':1,'kind':'command_lifecycle','lifecycle_id':lifecycle_id,'operation_id':operation_id,
 'result':{'artifact_refs':[push_output_ref],'cleanup_status':'NOT_REQUIRED','command_exit_status':proc.returncode,'completed_at_utc':push_completed,
 'final_status':'SUCCEEDED','outputs_created':[push_output_ref],'started_at_utc':push_started,'termination_reason':'completed'},'retry_index':0,'task_id':TASK,'task_revision':REV}
write(ROOT/command_record_ref,canonical(command_lifecycle))
operation={'candidate_sha':head,'command_record_ref':command_record_ref,'credential_class':'local_user','deadline_at_utc':deadline,'format_version':1,
 'intervening_events':[],'kind':'push_operation','lifecycle_id':lifecycle_id,'lifecycle_started_at_utc':repo_start,'postflight':post,'preflight':pre,
 'push_completed_at_utc':push_completed,'push_operation_id':operation_id,'push_started_at_utc':push_started,'retry_index':0}
write(ROOT/(OPREF/'push-operation.json'),canonical(operation))
print(json.dumps({'candidate_sha':head,'parent_sha':parent,'remote_sha':post_sha,'push_exit':proc.returncode,'operation_id':operation_id,'lifecycle_id':lifecycle_id,'destination_observation_ref':(OPREF/'destination-observation.json').as_posix(),'push_operation_ref':(OPREF/'push-operation.json').as_posix()},sort_keys=True))
