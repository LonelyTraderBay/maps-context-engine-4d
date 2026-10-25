$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$repoRoot=(Resolve-Path -LiteralPath '.').Path
$supportRoot=Join-Path $repoRoot 'artifacts\git-delivery-support\T085-r1-33499f70-9889-4f8b-8776-9b46555825e5'
$opsRelative='archive-view/operations/payload-push'
$opsRoot=Join-Path $supportRoot ($opsRelative -replace '/','\')
$utf8=[Text.UTF8Encoding]::new($false)
$candidate='29a3ea41eb00fc17e20d52460d704c7738679a6c'
$api='https://api.github.com/repos/LonelyTraderBay/maps-context-engine-4d'
$ref='refs/heads/main'
$headers=@{'User-Agent'='MCE4D-T085-Postflight';Accept='application/vnd.github+json';'X-GitHub-Api-Version'='2022-11-28';'Cache-Control'='no-cache';Pragma='no-cache'}
function Get-UtcInstant { [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'",[Globalization.CultureInfo]::InvariantCulture) }
function Write-JsonFile([string]$Path,[object]$Value) { $tmp=$Path+'.tmp-'+[Guid]::NewGuid().ToString('N'); [IO.File]::WriteAllText($tmp,(ConvertTo-Json -InputObject $Value -Depth 100)+[Environment]::NewLine,$utf8); Move-Item -LiteralPath $tmp -Destination $Path -Force }
function Save-JsonText([string]$Path,[string]$Text) { [IO.File]::WriteAllText($Path,$Text,$utf8) }
$opsFull=(Resolve-Path -LiteralPath $opsRoot).Path
if(-not $opsFull.StartsWith($supportRoot.TrimEnd('\')+'\',[StringComparison]::OrdinalIgnoreCase)){throw 'Evidence path escaped support root.'}
$oldPostPath=Join-Path $opsRoot 'postflight.json'
$oldRepoPath=Join-Path $opsRoot 'github-repository-postflight.json'
$oldBranchPath=Join-Path $opsRoot 'github-branch-postflight.json'
$attempt2Post=Join-Path $opsRoot 'postflight-attempt-2-cachebusted-success.json'
$attempt2Repo=Join-Path $opsRoot 'github-repository-postflight-attempt-2-cachebusted-success.json'
$attempt2Branch=Join-Path $opsRoot 'github-branch-postflight-attempt-2-cachebusted-success.json'
if(-not (Test-Path $attempt2Post)){Copy-Item $oldPostPath $attempt2Post}
if(-not (Test-Path $attempt2Repo)){Copy-Item $oldRepoPath $attempt2Repo}
if(-not (Test-Path $attempt2Branch)){Copy-Item $oldBranchPath $attempt2Branch}
$attempt2Projection=Get-Content $attempt2Post -Raw|ConvertFrom-Json -AsHashtable -DateKind String
$attempt2BranchBody=Get-Content $attempt2Branch -Raw|ConvertFrom-Json -AsHashtable -DateKind String
Write-JsonFile (Join-Path $opsRoot 'postflight-attempt-2-classification.json') ([ordered]@{
    result='API_BRANCH_CONFIRMED_CANDIDATE_BUT_LIFECYCLE_FINALIZATION_FAILED'
    query_id=$attempt2Projection.query_id
    query_started_at_utc=$attempt2Projection.started_at_utc
    query_completed_at_utc=$attempt2Projection.completed_at_utc
    repository_id=1421031011
    visibility='public'
    branch_api_sha=$attempt2BranchBody.commit.sha
    observed_remote_sha=$attempt2Projection.observed_remote_sha
    failure='The query and body were received and the script passed its verification gate, then local metadata serialization stopped because Invoke-WebRequest with OutFile did not return a response object.'
    superseded_by='a third cache-busted query with retained response metadata'
})
$scriptPath=Join-Path $repoRoot 'work\T085-r1-implementation\finalize_postflight_v3.ps1'
$scriptEvidencePath=Join-Path $opsRoot 'finalize-postflight-v3.ps1'
Copy-Item -LiteralPath $scriptPath -Destination $scriptEvidencePath -Force
$started=Get-UtcInstant
$nonce=[Guid]::NewGuid().ToString('N')
$repoUrl=$api+'?mce4d_nonce='+$nonce
$branchUrl=$api+'/branches/main?mce4d_nonce='+$nonce
$repoResponse=Invoke-WebRequest -Uri $repoUrl -Headers $headers -TimeoutSec 15
$branchResponse=Invoke-WebRequest -Uri $branchUrl -Headers $headers -TimeoutSec 15
$repoText=[string]$repoResponse.Content
$branchText=[string]$branchResponse.Content
$repoBody=$repoText|ConvertFrom-Json -AsHashtable -DateKind String
$branchBody=$branchText|ConvertFrom-Json -AsHashtable -DateKind String
$remoteOutput=@(git ls-remote origin $ref 2>&1)
$remoteExit=$LASTEXITCODE
$remoteLine=($remoteOutput|Out-String).Trim()
$remoteSha=$null
if($remoteExit -eq 0 -and $remoteLine){$remoteSha=($remoteLine -split '\s+')[0]}
$completed=Get-UtcInstant
$verified=($repoResponse.StatusCode -eq 200 -and $branchResponse.StatusCode -eq 200 -and
    $repoBody.id -eq 1421031011 -and $repoBody.full_name -eq 'LonelyTraderBay/maps-context-engine-4d' -and
    $repoBody.visibility -eq 'public' -and $repoBody.private -eq $false -and $repoBody.archived -eq $false -and
    $repoBody.default_branch -eq 'main' -and $branchBody.name -eq 'main' -and $branchBody.protected -eq $false -and
    $branchBody.commit.sha -eq $candidate -and $remoteExit -eq 0 -and $remoteSha -eq $candidate)
if(-not $verified){
    Save-JsonText (Join-Path $opsRoot 'github-repository-postflight-attempt-3-unresolved.json') $repoText
    Save-JsonText (Join-Path $opsRoot 'github-branch-postflight-attempt-3-unresolved.json') $branchText
    Write-JsonFile (Join-Path $opsRoot 'postflight-attempt-3-unresolved.json') ([ordered]@{
        query_started_at_utc=$started;query_completed_at_utc=$completed;repository_id=$repoBody.id
        repository_full_name=$repoBody.full_name;visibility=$repoBody.visibility;branch_sha=$branchBody.commit.sha
        git_ls_remote_sha=$remoteSha;candidate_sha=$candidate;result='UNKNOWN'
    })
    Write-Output 'POSTFLIGHT_REMAINS_UNKNOWN'
    exit 43
}
$queryId=[Guid]::NewGuid().ToString().ToLowerInvariant()
$plan=Get-Content (Join-Path $repoRoot 'tools\ci\check-plan.json') -Raw|ConvertFrom-Json -AsHashtable -DateKind String
$remoteConfig=Get-Content (Join-Path $opsRoot 'remote-config.json') -Raw|ConvertFrom-Json -AsHashtable -DateKind String
$projection=[ordered]@{
    query_id=$queryId
    lifecycle_id='115e0cef-0328-403a-bff1-b79972b6c47c'
    retry_index=0
    candidate_sha=$candidate
    repository=$plan.repository
    remote_config_digest=$remoteConfig.sha256
    started_at_utc=$started
    completed_at_utc=$completed
    observed_remote_sha=$branchBody.commit.sha
}
Save-JsonText $oldRepoPath $repoText
Save-JsonText $oldBranchPath $branchText
Write-JsonFile $oldPostPath $projection
Write-JsonFile (Join-Path $opsRoot 'postflight-query-metadata.json') ([ordered]@{
    query_id=$queryId
    query_started_at_utc=$started
    query_completed_at_utc=$completed
    request_nonce=$nonce
    repository_api_url=$repoUrl
    branch_api_url=$branchUrl
    request_headers=@{cache_control='no-cache';pragma='no-cache';github_api_version='2022-11-28'}
    repository_http_status=$repoResponse.StatusCode
    branch_http_status=$branchResponse.StatusCode
    repository_date=[string]$repoResponse.Headers['Date']
    branch_date=[string]$branchResponse.Headers['Date']
    repository_cache_control=[string]$repoResponse.Headers['Cache-Control']
    branch_cache_control=[string]$branchResponse.Headers['Cache-Control']
    repository_age=[string]$repoResponse.Headers['Age']
    branch_age=[string]$branchResponse.Headers['Age']
    repository_request_id=[string]$repoResponse.Headers['X-GitHub-Request-Id']
    branch_request_id=[string]$branchResponse.Headers['X-GitHub-Request-Id']
    repository_body_sha256=(Get-FileHash $oldRepoPath -Algorithm SHA256).Hash.ToLowerInvariant()
    branch_body_sha256=(Get-FileHash $oldBranchPath -Algorithm SHA256).Hash.ToLowerInvariant()
    git_ls_remote_sha=$remoteSha
})
Write-JsonFile (Join-Path $opsRoot 'postflight-attempt-1-classification.json') ([ordered]@{
    result='STALE_API_RESPONSE_EXCLUDED_FROM_CURRENT_POSTFLIGHT'
    previous_api_branch_sha='9813fe7299ff3b103531e8350614f27b1737063d'
    previous_git_ls_remote_sha=$candidate
    finding='The first postflight branch API body lagged the just-completed push. Its raw response and projection are retained; the current postflight uses a later unique-nonce no-cache request that matches git ls-remote.'
    superseded_by_query_id=$queryId
})
$operationPath=Join-Path $opsRoot 'push-operation.json'
$operation=Get-Content $operationPath -Raw|ConvertFrom-Json -AsHashtable -DateKind String
$operation.postflight=[ordered]@{
    query_id=$queryId;lifecycle_id=$operation.lifecycle_id;retry_index=$operation.retry_index
    candidate_sha=$candidate;repository=$plan.repository;remote_config_digest=$remoteConfig.sha256
    started_at_utc=$started;completed_at_utc=$completed;observed_remote_sha=$branchBody.commit.sha
    raw_api_ref='archive-view/operations/payload-push/postflight.json'
}
Write-JsonFile $operationPath $operation
$destinationPath=Join-Path $opsRoot 'destination-observation.json'
$destination=Get-Content $destinationPath -Raw|ConvertFrom-Json -AsHashtable -DateKind String
$destination.identity_verified=$true
$destination.visibility_verified=$true
Write-JsonFile $destinationPath $destination
$artifacts=[string[]]@(
'archive-view/operations/payload-push/github-tool-repo-observation.json',
'archive-view/operations/payload-push/remote-config.json',
'archive-view/operations/payload-push/github-repository-preflight.json',
'archive-view/operations/payload-push/github-branch-preflight.json',
'archive-view/operations/payload-push/preflight.json',
'archive-view/operations/payload-push/push-output.txt',
'archive-view/operations/payload-push/github-repository-postflight-attempt-1.json',
'archive-view/operations/payload-push/github-branch-postflight-attempt-1-stale.json',
'archive-view/operations/payload-push/postflight-attempt-1-stale.json',
'archive-view/operations/payload-push/postflight-attempt-1-classification.json',
'archive-view/operations/payload-push/github-repository-postflight-attempt-2-cachebusted-success.json',
'archive-view/operations/payload-push/github-branch-postflight-attempt-2-cachebusted-success.json',
'archive-view/operations/payload-push/postflight-attempt-2-cachebusted-success.json',
'archive-view/operations/payload-push/postflight-attempt-2-classification.json',
'archive-view/operations/payload-push/github-repository-postflight.json',
'archive-view/operations/payload-push/github-branch-postflight.json',
'archive-view/operations/payload-push/postflight.json',
'archive-view/operations/payload-push/postflight-query-metadata.json',
'archive-view/operations/payload-push/push-operation.json',
'archive-view/operations/payload-push/destination-observation.json',
'archive-view/operations/payload-push/command-lifecycle.json',
'archive-view/operations/payload-push/retention.json',
'archive-view/operations/payload-push/push-driver.ps1',
'archive-view/operations/payload-push/reconcile-postflight.ps1',
'archive-view/operations/payload-push/finalize-postflight-v3.ps1'
)
Write-JsonFile (Join-Path $opsRoot 'retention.json') ([ordered]@{
    format_version=1;action='retain'
    reason='Retain the first stale postflight, the cache-busted reconciliation and the final typed lifecycle for S21 archive/readback.'
    retained_paths=$artifacts
})
$commandPath=Join-Path $opsRoot 'command-lifecycle.json'
$command=Get-Content $commandPath -Raw|ConvertFrom-Json -AsHashtable -DateKind String
$command.contract.expected_writes=$artifacts
$command.result.completed_at_utc=Get-UtcInstant
$command.result.artifact_refs=$artifacts
Write-JsonFile $commandPath $command
Write-Output (ConvertTo-Json -InputObject ([ordered]@{
    result='POSTFLIGHT_RECONCILED';query_id=$queryId;repository_id=$repoBody.id
    visibility=$repoBody.visibility;branch_api_sha=$branchBody.commit.sha;git_ls_remote_sha=$remoteSha
    destination_verified=$verified;operation_id=$operation.push_operation_id
}) -Compress)
