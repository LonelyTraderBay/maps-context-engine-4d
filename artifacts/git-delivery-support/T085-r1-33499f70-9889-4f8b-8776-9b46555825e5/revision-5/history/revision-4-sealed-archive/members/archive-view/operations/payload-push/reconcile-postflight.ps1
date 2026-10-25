$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repoRoot = (Resolve-Path -LiteralPath '.').Path
$supportRoot = Join-Path $repoRoot 'artifacts\git-delivery-support\T085-r1-33499f70-9889-4f8b-8776-9b46555825e5'
$opsRelative = 'archive-view/operations/payload-push'
$opsRoot = Join-Path $supportRoot ($opsRelative -replace '/', '\')
$utf8 = [Text.UTF8Encoding]::new($false)
$candidate = '29a3ea41eb00fc17e20d52460d704c7738679a6c'
$ref = 'refs/heads/main'
$api = 'https://api.github.com/repos/LonelyTraderBay/maps-context-engine-4d'
$headers = @{ 'User-Agent'='MCE4D-T085-Postflight'; Accept='application/vnd.github+json'; 'X-GitHub-Api-Version'='2022-11-28'; 'Cache-Control'='no-cache'; Pragma='no-cache' }
function Get-UtcInstant { [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture) }
function Write-JsonFile([string]$Path,[object]$Value) {
    $tmp = $Path + '.tmp-' + [Guid]::NewGuid().ToString('N')
    [IO.File]::WriteAllText($tmp,(ConvertTo-Json -InputObject $Value -Depth 100)+[Environment]::NewLine,$utf8)
    Move-Item -LiteralPath $tmp -Destination $Path -Force
}
function Get-Api([string]$Uri,[string]$Path) {
    $response = Invoke-WebRequest -Uri $Uri -Headers $headers -TimeoutSec 15 -OutFile $Path
    $body = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -AsHashtable -DateKind String
    return @{ response=$response; body=$body }
}
$opsRootReal = (Resolve-Path -LiteralPath $opsRoot).Path
$rootPrefix = $supportRoot.TrimEnd('\') + '\'
if (-not $opsRootReal.StartsWith($rootPrefix,[StringComparison]::OrdinalIgnoreCase)) { throw 'Evidence path escaped support root.' }
$remoteConfig = Get-Content -LiteralPath (Join-Path $opsRoot 'remote-config.json') -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$currentFetch = (git remote get-url origin).Trim()
$currentPush = (git remote get-url --push origin).Trim()
$storedDigest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($utf8.GetBytes($remoteConfig.canonical_payload))).ToLowerInvariant()
if ($storedDigest -ne $remoteConfig.sha256 -or $currentFetch -ne 'https://github.com/LonelyTraderBay/maps-context-engine-4d.git' -or $currentPush -ne $currentFetch) { throw 'Remote configuration differs from retained push observation.' }
$operationPath = Join-Path $opsRoot 'push-operation.json'
$operation = Get-Content -LiteralPath $operationPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
if ($operation.candidate_sha -ne $candidate -or $operation.push_operation_id -ne 'e687c9bb-7d96-4e28-9a6e-1dd8eb127d1e') { throw 'Unexpected payload push operation identity.' }
$stalePostPath = Join-Path $opsRoot 'postflight.json'
$staleBranchPath = Join-Path $opsRoot 'github-branch-postflight.json'
$staleRepoPath = Join-Path $opsRoot 'github-repository-postflight.json'
Copy-Item -LiteralPath $stalePostPath -Destination (Join-Path $opsRoot 'postflight-attempt-1-stale.json') -Force
Copy-Item -LiteralPath $staleBranchPath -Destination (Join-Path $opsRoot 'github-branch-postflight-attempt-1-stale.json') -Force
Copy-Item -LiteralPath $staleRepoPath -Destination (Join-Path $opsRoot 'github-repository-postflight-attempt-1.json') -Force
$staleProjection = Get-Content -LiteralPath $stalePostPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$staleBranch = Get-Content -LiteralPath $staleBranchPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$reconcilePath = Join-Path $opsRoot 'reconcile-postflight.ps1'
Copy-Item -LiteralPath (Join-Path $repoRoot 'work\T085-r1-implementation\reconcile_postflight.ps1') -Destination $reconcilePath -Force
$started = Get-UtcInstant
$nonce = [Guid]::NewGuid().ToString('N')
$repoUrl = $api + '?mce4d_nonce=' + $nonce
$branchUrl = $api + '/branches/main?mce4d_nonce=' + $nonce
$repoPath = Join-Path $opsRoot 'github-repository-postflight.json'
$branchPath = Join-Path $opsRoot 'github-branch-postflight.json'
$repoResponse = Get-Api $repoUrl $repoPath
$branchResponse = Get-Api $branchUrl $branchPath
$remoteOutput = @(git ls-remote origin $ref 2>&1)
$remoteExit = $LASTEXITCODE
$remoteLine = ($remoteOutput | Out-String).Trim()
$remoteSha = $null
if ($remoteExit -eq 0 -and $remoteLine) { $remoteSha = ($remoteLine -split '\s+')[0] }
$completed = Get-UtcInstant
$repoBody = $repoResponse.body
$branchBody = $branchResponse.body
$verified = ($repoBody.id -eq 1421031011 -and $repoBody.full_name -eq 'LonelyTraderBay/maps-context-engine-4d' -and
    $repoBody.visibility -eq 'public' -and $repoBody.private -eq $false -and $repoBody.archived -eq $false -and
    $repoBody.default_branch -eq 'main' -and $branchBody.name -eq 'main' -and $branchBody.protected -eq $false -and
    $branchBody.commit.sha -eq $candidate -and $remoteExit -eq 0 -and $remoteSha -eq $candidate)
if (-not $verified) {
    Write-JsonFile (Join-Path $opsRoot 'postflight-attempt-2-unresolved.json') ([ordered]@{
        query_started_at_utc=$started; query_completed_at_utc=$completed
        repository_id=$repoBody.id; repository_full_name=$repoBody.full_name; visibility=$repoBody.visibility
        branch_sha=$branchBody.commit.sha; git_ls_remote_sha=$remoteSha; candidate_sha=$candidate
        result='UNKNOWN'
    })
    Write-Output 'POSTFLIGHT_REMAINS_UNKNOWN'
    exit 43
}
$queryId = [Guid]::NewGuid().ToString().ToLowerInvariant()
$plan = Get-Content -LiteralPath (Join-Path $repoRoot 'tools\ci\check-plan.json') -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$postflight = [ordered]@{
    query_id=$queryId
    lifecycle_id=$operation.lifecycle_id
    retry_index=$operation.retry_index
    candidate_sha=$candidate
    repository=$plan.repository
    remote_config_digest=$remoteConfig.sha256
    started_at_utc=$started
    completed_at_utc=$completed
    observed_remote_sha=$branchBody.commit.sha
}
Write-JsonFile (Join-Path $opsRoot 'postflight.json') $postflight
$headersMeta = [ordered]@{
    query_id=$queryId
    query_started_at_utc=$started
    query_completed_at_utc=$completed
    cache_control='no-cache, Pragma: no-cache'
    request_nonce=$nonce
    repository_api_url=$repoUrl
    branch_api_url=$branchUrl
    repository_http_status=$repoResponse.response.StatusCode
    branch_http_status=$branchResponse.response.StatusCode
    repository_date=[string]$repoResponse.response.Headers['Date']
    branch_date=[string]$branchResponse.response.Headers['Date']
    repository_cache_control=[string]$repoResponse.response.Headers['Cache-Control']
    branch_cache_control=[string]$branchResponse.response.Headers['Cache-Control']
    repository_age=[string]$repoResponse.response.Headers['Age']
    branch_age=[string]$branchResponse.response.Headers['Age']
    repository_request_id=[string]$repoResponse.response.Headers['X-GitHub-Request-Id']
    branch_request_id=[string]$branchResponse.response.Headers['X-GitHub-Request-Id']
    repository_body_sha256=(Get-FileHash -LiteralPath $repoPath -Algorithm SHA256).Hash.ToLowerInvariant()
    branch_body_sha256=(Get-FileHash -LiteralPath $branchPath -Algorithm SHA256).Hash.ToLowerInvariant()
    git_ls_remote_sha=$remoteSha
}
Write-JsonFile (Join-Path $opsRoot 'postflight-query-metadata.json') $headersMeta
Write-JsonFile (Join-Path $opsRoot 'postflight-attempt-1-classification.json') ([ordered]@{
    result='RETAINED_STALE_API_RESPONSE_EXCLUDED_FROM_CURRENT_POSTFLIGHT'
    old_query_id=$staleProjection.query_id
    old_api_branch_sha=$staleBranch.commit.sha
    old_projection_observed_remote_sha=$staleProjection.observed_remote_sha
    finding='The first postflight API response still named the pre-push base while git ls-remote showed the pushed candidate; a cache-busted no-cache API query now confirms the candidate.'
    superseded_by_query_id=$queryId
    preserved_files=@('archive-view/operations/payload-push/postflight-attempt-1-stale.json','archive-view/operations/payload-push/github-branch-postflight-attempt-1-stale.json','archive-view/operations/payload-push/github-repository-postflight-attempt-1.json')
})
$operation.postflight = [ordered]@{
    query_id=$queryId
    lifecycle_id=$operation.lifecycle_id
    retry_index=$operation.retry_index
    candidate_sha=$candidate
    repository=$plan.repository
    remote_config_digest=$remoteConfig.sha256
    started_at_utc=$started
    completed_at_utc=$completed
    observed_remote_sha=$branchBody.commit.sha
    raw_api_ref='archive-view/operations/payload-push/postflight.json'
}
Write-JsonFile $operationPath $operation
$destinationPath = Join-Path $opsRoot 'destination-observation.json'
$destination = Get-Content -LiteralPath $destinationPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$destination.identity_verified=$true
$destination.visibility_verified=$true
Write-JsonFile $destinationPath $destination
$artifacts = [string[]]@(
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
    'archive-view/operations/payload-push/github-repository-postflight.json',
    'archive-view/operations/payload-push/github-branch-postflight.json',
    'archive-view/operations/payload-push/postflight.json',
    'archive-view/operations/payload-push/postflight-query-metadata.json',
    'archive-view/operations/payload-push/push-operation.json',
    'archive-view/operations/payload-push/destination-observation.json',
    'archive-view/operations/payload-push/command-lifecycle.json',
    'archive-view/operations/payload-push/retention.json',
    'archive-view/operations/payload-push/push-driver.ps1',
    'archive-view/operations/payload-push/reconcile-postflight.ps1'
)
Write-JsonFile (Join-Path $opsRoot 'retention.json') ([ordered]@{
    format_version=1
    action='retain'
    reason='Preserve the original stale postflight API response and the cache-busted successful reconciliation with the push lifecycle for S21 archive/readback.'
    retained_paths=$artifacts
})
$commandPath = Join-Path $opsRoot 'command-lifecycle.json'
$command = Get-Content -LiteralPath $commandPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$command.contract.expected_writes=$artifacts
$command.result.completed_at_utc=Get-UtcInstant
$command.result.artifact_refs=$artifacts
Write-JsonFile $commandPath $command
Write-Output (ConvertTo-Json -InputObject ([ordered]@{
    result='POSTFLIGHT_RECONCILED'
    query_id=$queryId
    repository_id=$repoBody.id
    visibility=$repoBody.visibility
    branch_api_sha=$branchBody.commit.sha
    git_ls_remote_sha=$remoteSha
    destination_verified=$verified
    operation_id=$operation.push_operation_id
}) -Compress)
