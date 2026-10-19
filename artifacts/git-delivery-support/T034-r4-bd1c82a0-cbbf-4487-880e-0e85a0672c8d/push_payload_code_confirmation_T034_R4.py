from __future__ import annotations
import hashlib,json,re,subprocess,sys,uuid
from datetime import datetime,timedelta,timezone
from pathlib import Path
ROOT=Path.cwd(); sys.path.insert(0,str(ROOT)); DELIVERY='bd1c82a0-cbbf-4487-880e-0e85a0672c8d'; TASK='T034'; REV=4
BASE='16136bf18b8d0e77ede1500ccf57399391addcd5'; REMOTE_BASE='6827429e6360741ceb8a42db94a5b4b0778b4991'; CANDIDATE=subprocess.check_output(['git','rev-parse','HEAD'],cwd=Path.cwd(),text=True,encoding='utf-8').strip()
STAGE=ROOT/'artifacts/git-delivery-support/T034-r4-bd1c82a0-cbbf-4487-880e-0e85a0672c8d'; REL='archive-view/operations/payload-code-confirmation-1'; OUT=STAGE/REL
OWNER='LonelyTraderBay'; NAME='maps-context-engine-4d'; REF='refs/heads/main'; REMOTE='origin'; REPO_ID=1421031011
EXPECTED_PATHS=['tests/acceptance/self_index_test.go']
REPOSITORY={'host':'github.com','owner':OWNER,'name':NAME,'repository_id':REPO_ID,'visibility':'public','ref':REF,'remote_name':REMOTE,'remote_url_sanitized':f'https://github.com/{OWNER}/{NAME}.git','publication_authority_ref':'authority/task-start.txt'}
ENDPOINTS={'user':'/user','repository':f'/repos/{OWNER}/{NAME}','ref':f'/repos/{OWNER}/{NAME}/git/ref/heads/main','branch':f'/repos/{OWNER}/{NAME}/branches/main','branch_protection':f'/repos/{OWNER}/{NAME}/branches/main/protection','rulesets':f'/repos/{OWNER}/{NAME}/rulesets?per_page=100','actions_permissions':f'/repos/{OWNER}/{NAME}/actions/permissions','collaborator_permission':f'/repos/{OWNER}/{NAME}/collaborators/{OWNER}/permission'}
def now():return datetime.now(timezone.utc)
def stamp(value=None):return (value or now()).isoformat(timespec='microseconds').replace('+00:00','Z')
def run(argv,check=True):
 p=subprocess.run(argv,cwd=ROOT,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=30)
 if check and p.returncode:raise RuntimeError(f'{argv[0]} exited {p.returncode}: {p.stderr.decode("utf-8","replace")[:500]}')
 return p
def put(ref,value):
 path=STAGE/ref;path.parent.mkdir(parents=True,exist_ok=True);path.write_text(json.dumps(value,sort_keys=True,separators=(',',':'),ensure_ascii=False)+'\n',encoding='utf-8')
def project(label,body):
 if label=='user':return {k:body[k] for k in ('id','login') if k in body}
 if label=='repository':
  owner=body.get('owner') if isinstance(body.get('owner'),dict) else {}
  return {k:v for k,v in {'id':body.get('id'),'full_name':body.get('full_name'),'name':body.get('name'),'owner':owner.get('login'),'private':body.get('private'),'visibility':body.get('visibility'),'default_branch':body.get('default_branch'),'archived':body.get('archived'),'disabled':body.get('disabled'),'permissions':body.get('permissions')}.items() if v is not None}
 if label=='ref':
  obj=body.get('object') if isinstance(body.get('object'),dict) else {}
  return {'ref':body.get('ref'),'object':{k:obj[k] for k in ('sha','type','url') if obj.get(k) is not None}}
 if label=='branch':
  commit=body.get('commit') if isinstance(body.get('commit'),dict) else {}
  return {'name':body.get('name'),'commit':{k:commit[k] for k in ('sha','url') if commit.get(k) is not None},'protected':body.get('protected')}
 if label=='actions_permissions':return {k:body.get(k) for k in ('enabled','allowed_actions','selected_actions_url') if k in body}
 if label=='collaborator_permission':
  user=body.get('user') if isinstance(body.get('user'),dict) else {}
  return {'permission':body.get('permission'),'user':{k:user[k] for k in ('id','login') if user.get(k) is not None}}
 if label=='rulesets':return body
 return {k:body.get(k) for k in ('message','documentation_url','status') if k in body}
def query(label,phase,lifecycle,candidate,digest):
 started=now();proc=run(['gh','api','--include',ENDPOINTS[label]],check=False);completed=now()
 raw=proc.stdout.decode('utf-8','replace');matches=list(re.finditer(r'HTTP/\S+\s+(\d{3})',raw))
 if not matches:raise RuntimeError(f'No HTTP response for {label}: {proc.stderr.decode("utf-8","replace")[:300]}')
 status=int(matches[-1].group(1));body_text=raw[matches[-1].end():].split('\r\n\r\n',1)[-1].split('\n\n',1)[-1].strip()
 try:body=json.loads(body_text) if body_text else {}
 except json.JSONDecodeError as exc:raise RuntimeError(f'Invalid API JSON for {label}: {exc}')
 body=project(label,body);evidence={'query_label':f'{phase}-{label}','endpoint':ENDPOINTS[label],'started_at_utc':stamp(started),'completed_at_utc':stamp(completed),'exit_code':proc.returncode,'http_status':status,'body':body}
 if proc.stderr: evidence['error']=proc.stderr.decode('utf-8','replace')
 ref=f'{REL}/api/{phase}-{label}.body.json';put(ref,evidence)
 return evidence,ref
def collect(phase,qid,started,lifecycle,candidate,digest):
 results={};refs=[]
 for label in ENDPOINTS:
  ev,ref=query(label,phase,lifecycle,candidate,digest);results[label]={'body_ref':ref,'started_at_utc':ev['started_at_utc'],'completed_at_utc':ev['completed_at_utc'],'exit_code':ev['exit_code'],'http_status':ev['http_status']};refs.append(ref)
 proc=run(['git','ls-remote',REMOTE,REF],check=False);remote_sha=proc.stdout.decode().strip().split('\t')[0] if proc.returncode==0 and proc.stdout.strip() else None
 snap={'format_version':1,'query_id':qid,'lifecycle_id':lifecycle,'retry_index':0,'candidate_sha':candidate,'repository':REPOSITORY,'remote_config_digest':digest,'started_at_utc':stamp(started),'completed_at_utc':stamp(),'observed_remote_sha':remote_sha,'api_evidence_refs':refs,'query_results':results,'git_ls_remote':{'command':'git ls-remote origin refs/heads/main','ref':REF,'sha':remote_sha}}
 ref=f'{REL}/raw/{phase}-typed-query.json';put(ref,snap)
 projection_ref=f'{REL}/raw/{phase}-query-projection.json'
 projection_fields=('query_id','lifecycle_id','retry_index','candidate_sha','repository','remote_config_digest','started_at_utc','completed_at_utc','observed_remote_sha')
 put(projection_ref,{key:snap[key] for key in projection_fields});return snap,projection_ref
if run(['git','rev-parse','HEAD']).stdout.decode().strip()!=CANDIDATE or run(['git','branch','--show-current']).stdout.decode().strip()!='main':raise SystemExit('Candidate SHA/branch mismatch')
if run(['git','diff-index','--quiet','HEAD','--']).returncode or run(['git','diff','--cached','--quiet']).returncode:raise SystemExit('Tracked worktree or index is not clean')
message=run(['git','show','-s','--format=%B',CANDIDATE]).stdout.decode('utf-8').rstrip('\n')
# Existing commit is confirmed as historical payload; this invocation creates no commit.
# This fresh confirmation does not claim to have recorded the earlier transfer.
paths=run(['git','diff-tree','--no-commit-id','--name-only','-r',f'{BASE}..{CANDIDATE}']).stdout.decode().splitlines()
if paths != EXPECTED_PATHS: raise SystemExit(f'T034 r4 payload scope mismatch: {paths}')
plan_bytes=(STAGE/'check-plan.json').read_bytes(); plan=json.loads(plan_bytes)
from tools.spec_check.archive import _canonical
if plan_bytes != _canonical(plan) or hashlib.sha256(plan_bytes).hexdigest() != 'bc157681517301a6cdff3601707d09f2f5f5d8e8365fc05474d047100aa9b495': raise SystemExit('Frozen plan bytes/hash mismatch')
local=json.loads((STAGE/'local-checks/payload/attempt-2/receipt.json').read_text(encoding='utf-8'))
tree=run(['git','rev-parse',f'{CANDIDATE}^{{tree}}']).stdout.decode().strip()
if (local.get('task_revision') != REV or local.get('delivery_id') != DELIVERY or local.get('phase') != 'payload' or local.get('base_sha') != BASE or local.get('candidate_sha') != CANDIDATE or local.get('candidate_tree_sha') != tree or local.get('check_plan_sha256') != 'bc157681517301a6cdff3601707d09f2f5f5d8e8365fc05474d047100aa9b495' or local.get('result') != 'PASS' or local.get('passed_checks') != 11 or local.get('failed_checks') != 0 or local.get('check_count') != 11): raise SystemExit('Frozen 11/11 T034 payload checks are not PASS on exact commit/tree')
config={'branch.main.merge':run(['git','config','--get','branch.main.merge']).stdout.decode().strip(),'branch.main.remote':run(['git','config','--get','branch.main.remote']).stdout.decode().strip(),'remote.origin.fetch':run(['git','config','--get-all','remote.origin.fetch']).stdout.decode().splitlines(),'remote.origin.url':run(['git','config','--get-all','remote.origin.url']).stdout.decode().splitlines()}
config_digest=hashlib.sha256(json.dumps(config,sort_keys=True,separators=(',',':'),ensure_ascii=False).encode()).hexdigest()
if config['branch.main.remote']!='origin' or config['branch.main.merge']!=REF or config['remote.origin.url']!=[REPOSITORY['remote_url_sanitized']]:raise SystemExit('Git destination configuration mismatch')
source_git=run(['git','--version']).stdout.decode().strip();helper=run(['git','config','--get','credential.helper'],check=False).stdout.decode().strip() or 'unknown';gcm=run(['git','credential-manager','--version'],check=False).stdout.decode().strip() or 'unavailable'
remote_before=run(['git','ls-remote',REMOTE,REF]).stdout.decode().strip().split('\t')[0]
if remote_before!=REMOTE_BASE:raise SystemExit(f'origin/main moved: expected {REMOTE_BASE}, observed {remote_before}')
OUT.mkdir(parents=True,exist_ok=False);(OUT/'api').mkdir();(OUT/'raw').mkdir()
lifecycle=str(uuid.uuid4());operation=str(uuid.uuid4());command_id=str(uuid.uuid4());run_id=str(uuid.uuid4());pre_id=str(uuid.uuid4());post_id=str(uuid.uuid4())
life_start=now();deadline=life_start+timedelta(minutes=10)
pre,pre_ref=collect('preflight',pre_id,life_start,lifecycle,CANDIDATE,config_digest)
pre_bodies={key:json.loads((STAGE/value['body_ref']).read_text(encoding='utf-8'))['body'] for key,value in pre['query_results'].items()}
repository=pre_bodies['repository'];user=pre_bodies['user'];branch=pre_bodies['branch'];ref=pre_bodies['ref']
if user.get('login')!=OWNER or repository.get('id')!=REPO_ID or repository.get('full_name')!=f'{OWNER}/{NAME}' or repository.get('visibility')!='public' or repository.get('private') is not False or repository.get('archived') or repository.get('disabled'):raise SystemExit('Destination identity/visibility/state mismatch')
if pre['observed_remote_sha']!=REMOTE_BASE or branch.get('commit',{}).get('sha')!=REMOTE_BASE or ref.get('object',{}).get('sha')!=REMOTE_BASE:raise SystemExit('Fresh preflight main SHA mismatch')
if pre_bodies['branch_protection'].get('status')!='404' or pre_bodies['rulesets']!=[]:raise SystemExit('Branch protection/rulesets require review; push not sent')
if pre_bodies['actions_permissions'].get('enabled') is not True or pre_bodies['collaborator_permission'].get('permission') not in ('admin','write','maintain'):raise SystemExit('Actions or push permission is insufficient')
print('PREFLIGHT PASS; sending one normal exact-SHA T034 confirmation push to origin/main',flush=True)
push_start=now();argv=['push','--porcelain','origin',f'{CANDIDATE}:{REF}'];push=run(['git',*argv],check=False);push_end=now();(OUT/'push-output.txt').write_bytes(push.stdout+push.stderr)
if push.returncode:raise SystemExit(f'push failed with exit {push.returncode}; output retained')
post,post_ref=collect('postflight',post_id,push_end,lifecycle,CANDIDATE,config_digest)
post_bodies={key:json.loads((STAGE/value['body_ref']).read_text(encoding='utf-8'))['body'] for key,value in post['query_results'].items()}
if post['observed_remote_sha']!=CANDIDATE or post_bodies['ref'].get('object',{}).get('sha')!=CANDIDATE or post_bodies['branch'].get('commit',{}).get('sha')!=CANDIDATE:raise SystemExit('postflight SHA does not equal candidate')
command_source=f'{REL}/push-command-source.json';cleanup=f'{REL}/cleanup-plan.json';retention=f'{REL}/retention.json';command_ref=f'{REL}/command-lifecycle.json';operation_ref=f'{REL}/push-operation.json';destination_ref=f'{REL}/destination-observation.json'
put(command_source,{'format_version':1,'kind':'push_command_source','executable':'git','argv':['git',*argv],'working_directory_ref':'.','git_version':source_git,'credential_provider':f'git credential.helper={helper}','credential_provider_version':gcm,'credential_material_recorded':False})
put(cleanup,{'format_version':1,'kind':'cleanup_plan','operation_id':operation,'decision':'NOT_REQUIRED','reason':'T034 r4 code payload exact-SHA confirmation push không tạo disposable output.'})
put(retention,{'format_version':1,'kind':'retention_record','delivery_id':DELIVERY,'authority_ref':'authority/task-start.txt','preserve_until':'task-closure-and-archive-readback'})
patch=run(['git','show','--pretty=format:','--binary',CANDIDATE]).stdout;patch_hash=hashlib.sha256(patch).hexdigest()
contract={'command_id':command_id,'task_mode':'implement','owner':'Codex','baseline_ref':f'git:{BASE}','executable':'git','argv':argv,'cwd':'.','tool_versions':{'git':source_git,'gh':run(['gh','--version']).stdout.decode().splitlines()[0],'git_credential_manager':gcm},'command_source_ref':command_source,'input_refs':['check-plan.json','authority/task-start.txt',f'git:{BASE}',f'git:{CANDIDATE}'],'contracts':['PUSH-FRESH-01','GIT-05','GIT-06','COMMAND-LIFECYCLE-01'],'fingerprints':{'candidate_commit_diff':patch_hash,'remote_config':config_digest},'expected_writes':[f'Xác nhận code payload T034 revision 4 {CANDIDATE} trên refs/heads/main bằng một push exact-SHA không đổi lịch sử.'],'temporary_roots':[],'cache_roots':[],'log_roots':[f'{REL}/push-output.txt'],'download_roots':[],'network_scope':['github.com','api.github.com'],'child_processes':[],'side_effect_class':'external_service','permission_ref':'authority/task-start.txt','storage_mode':'bounded_small','volumes':[],'budget_refs':[],'retention_ref':retention,'deadline_at_utc':stamp(deadline),'monitor_interval_ms':1000,'run_id':run_id,'output_root':REL,'cleanup_plan_ref':cleanup,'cleanup_permission_ref':None}
put(command_ref,{'format_version':1,'kind':'command_lifecycle','operation_id':operation,'lifecycle_id':lifecycle,'retry_index':0,'assignment_ref':'S53/AW-01/AW-08:user-assignment-2026-10-15','task_id':TASK,'task_revision':REV,'delivery_id':DELIVERY,'command_state':'SUCCEEDED','contract':contract,'result':{'started_at_utc':stamp(push_start),'completed_at_utc':post['completed_at_utc'],'command_exit_status':0,'termination_reason':'completed','cleanup_status':'NOT_REQUIRED','final_status':'SUCCEEDED','outputs_created':[f'{REL}/push-output.txt'],'artifact_refs':[f'{REL}/push-output.txt']}})
preprojection={k:v for k,v in pre.items() if k not in ('format_version','api_evidence_refs','query_results','git_ls_remote')};postprojection={k:v for k,v in post.items() if k not in ('format_version','api_evidence_refs','query_results','git_ls_remote')}
put(operation_ref,{'format_version':1,'kind':'push_operation','push_operation_id':operation,'lifecycle_id':lifecycle,'retry_index':0,'candidate_sha':CANDIDATE,'credential_class':'local_user','command_record_ref':command_ref,'lifecycle_started_at_utc':stamp(life_start),'deadline_at_utc':stamp(deadline),'push_started_at_utc':stamp(push_start),'push_completed_at_utc':stamp(push_end),'intervening_events':[],'preflight':{**preprojection,'raw_api_ref':pre_ref},'postflight':{**postprojection,'raw_api_ref':post_ref}})
put(destination_ref,{'format_version':1,'push_operation_id':operation,'candidate_sha':CANDIDATE,'repository':REPOSITORY,'local_remote_config_digest':config_digest,'observed_at_utc':pre['completed_at_utc'],'push_started_at_utc':stamp(push_start),'identity_verified':True,'visibility_verified':True,'operation_evidence_ref':operation_ref})
print(json.dumps({'candidate_sha':CANDIDATE,'preflight_remote_sha':pre['observed_remote_sha'],'postflight_remote_sha':post['observed_remote_sha'],'push_exit':push.returncode,'operation_id':operation,'lifecycle_id':lifecycle,'result':'PASS','operation_ref':operation_ref},ensure_ascii=False),flush=True)




