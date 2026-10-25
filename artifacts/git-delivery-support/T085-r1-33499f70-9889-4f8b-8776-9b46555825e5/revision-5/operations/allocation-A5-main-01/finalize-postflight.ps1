$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$candidate = 'c04441ab75baac426492b8ad5be9167dcb1b0f3c'
$base = '2c3754a4dc8c20eacabf790b48def7225da3bcad'
$repoId = 1421031011
$repoFullName = 'LonelyTraderBay/maps-context-engine-4d'
$repoApi = 'https://api.github.com/repos/LonelyTraderBay/maps-context-engine-4d'
$ref = 'refs/heads/main'
$branchName = 'main'
$opsRelative = 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/revision-5/operations/allocation-A5-main-01'
$repoRoot = (Resolve-Path -LiteralPath '.').Path
$opsRoot = Join-Path $repoRoot ($opsRelative -replace '/', '\')
$utf8 = [Text.UTF8Encoding]::new($false)
$headers = @{ 'User-Agent'='MCE4D-T085-AW08'; 'Accept'='application/vnd.github+json'; 'X-GitHub-Api-Version'='2022-11-28'; 'Cache-Control'='no-cache'; 'Pragma'='no-cache' }

function Get-UtcInstant { [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture) }
function Write-JsonFile([string]$Path, [object]$Value) {
    [IO.File]::WriteAllText($Path, (ConvertTo-Json -InputObject $Value -Depth 100) + [Environment]::NewLine, $utf8)
}
function Save-JsonText([string]$Path, [string]$Text) { [IO.File]::WriteAllText($Path, $Text, $utf8) }

if ([IO.Path]::GetFullPath((git rev-parse --show-toplevel).Trim().Replace('/', '\')) -ne [IO.Path]::GetFullPath($repoRoot)) { throw 'Git root mismatch.' }
if ((git rev-parse HEAD).Trim() -ne $candidate) { throw 'Allocation candidate HEAD mismatch.' }
$opsFull = (Resolve-Path -LiteralPath $opsRoot).Path
$supportRoot = [IO.Path]::GetFullPath('artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5')
if (-not $opsFull.StartsWith([IO.Path]::GetFullPath($supportRoot).TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Evidence path escaped support root.' }

$oldPost = Join-Path $opsRoot 'postflight.json'
$oldRepo = Join-Path $opsRoot 'github-repository-postflight.json'
$oldBranch = Join-Path $opsRoot 'github-branch-postflight.json'
$attempt1Post = Join-Path $opsRoot 'postflight-attempt-1-stale.json'
$attempt1Repo = Join-Path $opsRoot 'github-repository-postflight-attempt-1-stale.json'
$attempt1Branch = Join-Path $opsRoot 'github-branch-postflight-attempt-1-stale.json'
if (-not (Test-Path -LiteralPath $attempt1Post)) { Copy-Item -LiteralPath $oldPost -Destination $attempt1Post }
if (-not (Test-Path -LiteralPath $attempt1Repo)) { Copy-Item -LiteralPath $oldRepo -Destination $attempt1Repo }
if (-not (Test-Path -LiteralPath $attempt1Branch)) { Copy-Item -LiteralPath $oldBranch -Destination $attempt1Branch }
$staleProjection = Get-Content $attempt1Post -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$staleBranchBody = Get-Content $attempt1Branch -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$permissionAttempt2 = Join-Path $opsRoot 'permission-snapshot-attempt-2.json'
if (-not (Test-Path -LiteralPath $permissionAttempt2)) {
    Copy-Item -LiteralPath 'work/T085-r1-implementation/github_repo_permission_a5_main_attempt2.json' -Destination $permissionAttempt2
}

$sourcePath = [IO.Path]::GetFullPath($MyInvocation.MyCommand.Path)
$sourceHash = (Get-FileHash -LiteralPath $sourcePath -Algorithm SHA256).Hash.ToLowerInvariant()
$sourceRef = $opsRelative + '/finalize-postflight.ps1'
$sourceEvidence = Join-Path $opsRoot 'finalize-postflight.ps1'
if (-not (Test-Path -LiteralPath $sourceEvidence)) { Copy-Item -LiteralPath $sourcePath -Destination $sourceEvidence }
if ((Get-FileHash -LiteralPath $sourceEvidence -Algorithm SHA256).Hash.ToLowerInvariant() -ne $sourceHash) { throw 'Retained reconciliation script differs from executed source.' }

$started = Get-UtcInstant
$nonce = [Guid]::NewGuid().ToString('N')
$repoUrl = $repoApi + '?mce4d_nonce=' + $nonce
$branchUrl = $repoApi + '/branches/main?mce4d_nonce=' + $nonce
$repoResponse = Invoke-WebRequest -Uri $repoUrl -Headers $headers -TimeoutSec 15
$branchResponse = Invoke-WebRequest -Uri $branchUrl -Headers $headers -TimeoutSec 15
$repoText = [string]$repoResponse.Content
$branchText = [string]$branchResponse.Content
$repoBody = $repoText | ConvertFrom-Json -AsHashtable -DateKind String
$branchBody = $branchText | ConvertFrom-Json -AsHashtable -DateKind String
$remoteOutput = @(git ls-remote origin $ref 2>&1)
$remoteExit = $LASTEXITCODE
$remoteLine = ($remoteOutput | Out-String).Trim()
$remoteSha = $null
if ($remoteExit -eq 0 -and $remoteLine) { $remoteSha = ($remoteLine -split '\s+')[0] }
$completed = Get-UtcInstant
$verified = ($repoResponse.StatusCode -eq 200 -and $branchResponse.StatusCode -eq 200 -and
    $repoBody.id -eq $repoId -and $repoBody.full_name -eq $repoFullName -and $repoBody.private -eq $false -and
    $repoBody.visibility -eq 'public' -and $repoBody.archived -eq $false -and $repoBody.default_branch -eq 'main' -and
    $branchBody.name -eq $branchName -and $branchBody.protected -eq $false -and
    $branchBody.commit.sha -eq $candidate -and $remoteExit -eq 0 -and $remoteSha -eq $candidate)

$attemptRepo = Join-Path $opsRoot 'github-repository-postflight-attempt-3-cachebusted-success.json'
$attemptBranch = Join-Path $opsRoot 'github-branch-postflight-attempt-3-cachebusted-success.json'
$attemptPost = Join-Path $opsRoot 'postflight-attempt-3-cachebusted-success.json'
Save-JsonText $attemptRepo $repoText
Save-JsonText $attemptBranch $branchText
if (-not $verified) {
    Write-JsonFile (Join-Path $opsRoot 'postflight-attempt-3-unresolved.json') ([ordered]@{
        query_started_at_utc=$started; query_completed_at_utc=$completed; request_nonce=$nonce
        repository_id=$repoBody.id; repository_full_name=$repoBody.full_name; visibility=$repoBody.visibility
        branch_sha=$branchBody.commit.sha; git_ls_remote_sha=$remoteSha; candidate_sha=$candidate; result='UNKNOWN'
    })
    Write-Output 'POSTFLIGHT_REMAINS_UNKNOWN'
    exit 43
}

$queryId = [Guid]::NewGuid().ToString().ToLowerInvariant()
$operationPath = Join-Path $opsRoot 'push-operation.json'
$operation = Get-Content $operationPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$remoteConfig = Get-Content (Join-Path $opsRoot 'remote-config.json') -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$projection = [ordered]@{
    query_id=$queryId; lifecycle_id=$operation.lifecycle_id; retry_index=0; candidate_sha=$candidate
    repository=$operation.preflight.repository; remote_config_digest=$remoteConfig.sha256
    started_at_utc=$started; completed_at_utc=$completed; observed_remote_sha=$remoteSha
}
$attemptProjection = [ordered]@{}
foreach ($entry in $projection.GetEnumerator()) { $attemptProjection.Add($entry.Key, $entry.Value) }
$attemptProjection.Add('raw_api_ref', $opsRelative + '/postflight-attempt-3-cachebusted-success.json')
Write-JsonFile $attemptPost $attemptProjection
Write-JsonFile (Join-Path $opsRoot 'postflight-attempt-3-classification.json') ([ordered]@{
    result='PASS_CACHEBUSTED_POSTFLIGHT'; query_id=$queryId; candidate_sha=$candidate
    branch_api_sha=$branchBody.commit.sha; git_ls_remote_sha=$remoteSha; verified=$verified; request_nonce=$nonce
})
Save-JsonText $oldRepo $repoText
Save-JsonText $oldBranch $branchText
Write-JsonFile $oldPost $projection
Write-JsonFile (Join-Path $opsRoot 'postflight-query-metadata.json') ([ordered]@{
    query_id=$queryId; query_started_at_utc=$started; query_completed_at_utc=$completed; request_nonce=$nonce
    repository_api_url=$repoUrl; branch_api_url=$branchUrl
    request_headers=@{cache_control='no-cache'; pragma='no-cache'; github_api_version='2022-11-28'}
    repository_http_status=$repoResponse.StatusCode; branch_http_status=$branchResponse.StatusCode
    repository_date=[string]$repoResponse.Headers['Date']; branch_date=[string]$branchResponse.Headers['Date']
    repository_cache_control=[string]$repoResponse.Headers['Cache-Control']; branch_cache_control=[string]$branchResponse.Headers['Cache-Control']
    repository_age=[string]$repoResponse.Headers['Age']; branch_age=[string]$branchResponse.Headers['Age']
    repository_request_id=[string]$repoResponse.Headers['X-GitHub-Request-Id']; branch_request_id=[string]$branchResponse.Headers['X-GitHub-Request-Id']
    repository_body_sha256=(Get-FileHash $oldRepo -Algorithm SHA256).Hash.ToLowerInvariant()
    branch_body_sha256=(Get-FileHash $oldBranch -Algorithm SHA256).Hash.ToLowerInvariant(); git_ls_remote_sha=$remoteSha
})
Write-JsonFile (Join-Path $opsRoot 'postflight-attempt-1-classification.json') ([ordered]@{
    result='STALE_API_RESPONSE_EXCLUDED_FROM_CURRENT_POSTFLIGHT'; query_id=$operation.postflight.query_id
    candidate_sha=$candidate; previous_api_branch_sha=$staleBranchBody.commit.sha; previous_git_ls_remote_sha=$staleProjection.observed_remote_sha
    git_push_exit_status=0; initial_driver_exit_status=43
    finding='The initial postflight API body returned the prior branch SHA while git ls-remote showed the completed push candidate. Its original raw response and projection are retained. A later unique-nonce no-cache query matched both.'
    superseded_by_query_id=$queryId
})
$operation.postflight = [ordered]@{
    query_id=$queryId; lifecycle_id=$operation.lifecycle_id; retry_index=$operation.retry_index
    candidate_sha=$candidate; repository=$operation.preflight.repository; remote_config_digest=$remoteConfig.sha256
    started_at_utc=$started; completed_at_utc=$completed; observed_remote_sha=$remoteSha
    raw_api_ref=$opsRelative + '/postflight.json'
}
Write-JsonFile $operationPath $operation
$destinationPath = Join-Path $opsRoot 'destination-observation.json'
$destination = Get-Content $destinationPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$destination.identity_verified = $true
$destination.visibility_verified = $true
Write-JsonFile $destinationPath $destination

$retentionPath = Join-Path $opsRoot 'retention.json'
$refs = @(
    $opsRelative+'/github-tool-repo-observation.json', $opsRelative+'/remote-config.json',
    $opsRelative+'/github-repository-preflight.json', $opsRelative+'/github-branch-preflight.json', $opsRelative+'/preflight.json', $opsRelative+'/push-output.txt',
    $opsRelative+'/github-repository-postflight-attempt-1-stale.json', $opsRelative+'/github-branch-postflight-attempt-1-stale.json', $opsRelative+'/postflight-attempt-1-stale.json', $opsRelative+'/postflight-attempt-1-classification.json',
    $opsRelative+'/github-branch-postflight-attempt-2-cachebusted-success.json', $opsRelative+'/postflight-attempt-2-classification.json',
    $opsRelative+'/github-repository-postflight-attempt-3-cachebusted-success.json', $opsRelative+'/github-branch-postflight-attempt-3-cachebusted-success.json', $opsRelative+'/postflight-attempt-3-cachebusted-success.json', $opsRelative+'/postflight-attempt-3-classification.json',
    $opsRelative+'/github-repository-postflight.json', $opsRelative+'/github-branch-postflight.json', $opsRelative+'/postflight.json', $opsRelative+'/postflight-query-metadata.json',
    $opsRelative+'/push-operation.json', $opsRelative+'/destination-observation.json', $opsRelative+'/command-lifecycle.json', $opsRelative+'/retention.json',
    $opsRelative+'/push-driver.ps1', $sourceRef, $opsRelative+'/preflight-freshness-attempt-1.json', $opsRelative+'/preflight-freshness-attempt-2.json',
    $opsRelative+'/permission-snapshot-attempt-1.json', $opsRelative+'/permission-snapshot-attempt-2.json'
)
Write-JsonFile $retentionPath ([ordered]@{ format_version=1; action='retain'; reason='Preserve original stale API projection, cache-busted reconciliation, and the complete A5 push lifecycle for S21 and sealed archive/readback.'; retained_paths=$refs })
$commandPath = Join-Path $opsRoot 'command-lifecycle.json'
$command = Get-Content $commandPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$command.command_state = 'SUCCEEDED'
$command.result.final_status = 'SUCCEEDED'
$command.result.completed_at_utc = $completed
$command.contract.expected_writes = $refs
$command.result.artifact_refs = $refs
Write-JsonFile $commandPath $command

Write-Output (ConvertTo-Json -InputObject ([ordered]@{ result='POSTFLIGHT_RECONCILED'; query_id=$queryId; repository_id=$repoBody.id; branch_api_sha=$branchBody.commit.sha; git_ls_remote_sha=$remoteSha; verified=$verified; push_operation_id=$operation.push_operation_id }) -Compress)
