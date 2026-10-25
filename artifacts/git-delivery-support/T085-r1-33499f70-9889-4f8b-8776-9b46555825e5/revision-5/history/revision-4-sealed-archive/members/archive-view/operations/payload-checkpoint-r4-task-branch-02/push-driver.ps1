$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repoRoot = (Resolve-Path -LiteralPath '.').Path
$supportRoot = Join-Path $repoRoot 'artifacts\git-delivery-support\T085-r1-33499f70-9889-4f8b-8776-9b46555825e5'
$opsRelative = 'archive-view/operations/payload-checkpoint-r4-main-02'
$opsRoot = Join-Path $supportRoot ($opsRelative -replace '/', '\')
$workRoot = Join-Path $repoRoot 'work\T085-r1-implementation'
$candidate = 'e45478b8b5a6ec4b6047f794c2b134acce03d486'
$base = 'acfdb8088516fac1cb4e190e2c6c1f5b3bddbba5'
$deliveryId = '711606c8-9642-44fc-905a-7ebfd8cfd686'
$taskId = 'T085'
$taskRevision = 4
$assignmentRef = 'S53/AW-01/AW-08:user-assignment-2026-10-15'
$ref = 'refs/heads/main'
$repoApi = 'https://api.github.com/repos/LonelyTraderBay/maps-context-engine-4d'
$headers = @{ 'User-Agent' = 'MCE4D-T085-AW08'; 'Accept' = 'application/vnd.github+json'; 'X-GitHub-Api-Version' = '2022-11-28' }
$utf8 = [System.Text.UTF8Encoding]::new($false)
$repoRootPrefix = $repoRoot.TrimEnd('\') + '\'

function Get-UtcInstant {
    [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)
}
function Write-JsonFile([string]$Path, [object]$Value) {
    $json = ConvertTo-Json -InputObject $Value -Depth 100
    [System.IO.File]::WriteAllText($Path, $json + [Environment]::NewLine, $utf8)
}
function Assert-Contained([string]$Path, [string]$Root) {
    $full = [System.IO.Path]::GetFullPath($Path)
    $prefix = [System.IO.Path]::GetFullPath($Root).TrimEnd('\') + '\'
    if (-not $full.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw 'Output path escaped the approved root.'
    }
}
function Ensure-PlainDirectory([string]$Path, [string]$Root) {
    Assert-Contained $Path $Root
    if (-not (Test-Path -LiteralPath $Path)) {
        New-Item -ItemType Directory -Path $Path -Force | Out-Null
    }
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'Output directory is a reparse point.'
    }
}
function Get-ApiSnapshot([string]$Uri, [string]$Path) {
    Invoke-WebRequest -Uri $Uri -Headers $headers -TimeoutSec 15 -OutFile $Path | Out-Null
    Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -AsHashtable
}

if ([IO.Path]::GetFullPath((git rev-parse --show-toplevel).Trim().Replace('/', '\')) -ne [IO.Path]::GetFullPath($repoRoot)) { throw 'Git root mismatch.' }
if ((git rev-parse HEAD).Trim() -ne $candidate) { throw 'Candidate HEAD mismatch.' }
if ((git branch --show-current).Trim() -ne 'codex/t085-r1-delivery') { throw 'Delivery branch mismatch.' }
git diff --quiet HEAD --
if ($LASTEXITCODE -ne 0) { throw 'Tracked working tree is not clean.' }
git merge-base --is-ancestor $base $candidate
if ($LASTEXITCODE -ne 0) { throw 'Candidate is not a descendant of the observed base.' }

$plan = Get-Content -LiteralPath (Join-Path $repoRoot 'artifacts\git-delivery-support\T085-r1-33499f70-9889-4f8b-8776-9b46555825e5\revision-4\check-plan-canonical.json') -Raw | ConvertFrom-Json -AsHashtable
if ($plan.repository.ref -ne $ref -or $plan.repository.remote_name -ne 'origin' -or
    $plan.repository.repository_id -ne 1421031011 -or $plan.repository.visibility -ne 'public') {
    throw 'Frozen plan destination mismatch.'
}
$authorityPath = Join-Path $supportRoot 'authority\task-start.txt'
$authority = Get-Content -LiteralPath $authorityPath -Raw
if (-not $authority.Contains('approved_delivery_scope: normal per-task commits and pushes to already verified origin/main')) {
    throw 'Task authority does not authorize this destination.'
}

Ensure-PlainDirectory $opsRoot $supportRoot
Ensure-PlainDirectory $workRoot $repoRoot
$sourcePath = Join-Path $workRoot 'push_driver_revision4_payload_checkpoint_q_main.ps1'
$evidenceSourcePath = Join-Path $opsRoot 'push-driver.ps1'
if (-not (Test-Path -LiteralPath $sourcePath)) { throw 'Push driver source is missing.' }
$sourceHash = (Get-FileHash -LiteralPath $sourcePath -Algorithm SHA256).Hash.ToLowerInvariant()
Copy-Item -LiteralPath $sourcePath -Destination $evidenceSourcePath -Force
if ((Get-FileHash -LiteralPath $evidenceSourcePath -Algorithm SHA256).Hash.ToLowerInvariant() -ne $sourceHash) {
    throw 'Retained command-source bytes differ from executed source.'
}

$permissionSnapshot = Get-Content -LiteralPath (Join-Path $workRoot 'github-tool-repo-observation-q-main-fresh.json') -Raw | ConvertFrom-Json -AsHashtable -DateKind String
$permissionSnapshotPath = Join-Path $opsRoot 'github-tool-repo-observation.json'
Copy-Item -LiteralPath (Join-Path $workRoot 'github-tool-repo-observation-q-main-fresh.json') -Destination $permissionSnapshotPath -Force
$permissionSnapshot = Get-Content -LiteralPath $permissionSnapshotPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
if ($permissionSnapshot.repository_id -ne 1421031011 -or
    $permissionSnapshot.repository_full_name -ne 'LonelyTraderBay/maps-context-engine-4d' -or
    $permissionSnapshot.visibility -ne 'public' -or
    $permissionSnapshot.default_branch -ne 'main' -or
    $permissionSnapshot.permissions.push -ne $true) {
    throw 'Fresh connected-repository identity, visibility, or push permission did not match.'
}
$permissionObserved = [DateTimeOffset]::Parse($permissionSnapshot.observed_at_utc, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::RoundtripKind)
if (([DateTimeOffset]::UtcNow - $permissionObserved).TotalSeconds -gt 30) {
    throw 'Connected-repository permission observation exceeded the 30 second preflight window.'
}

$fetchUrl = (git remote get-url origin).Trim()
$pushUrl = (git remote get-url --push origin).Trim()
$expectedUrl = 'https://github.com/LonelyTraderBay/maps-context-engine-4d.git'
$fetchUri = [Uri]::new($fetchUrl)
$pushUri = [Uri]::new($pushUrl)
if ($fetchUri.UserInfo -or $pushUri.UserInfo -or $fetchUrl -ne $expectedUrl -or $pushUrl -ne $expectedUrl) {
    throw 'Configured origin differs from the frozen, credential-free destination.'
}
$remoteConfig = [ordered]@{
    remote_name = 'origin'
    fetch_url_sanitized = $fetchUrl
    push_url_sanitized = $pushUrl
    target_ref = $ref
}
$remoteConfigPayload = ConvertTo-Json -InputObject $remoteConfig -Compress
$remoteConfigDigest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($utf8.GetBytes($remoteConfigPayload))).ToLowerInvariant()
Write-JsonFile (Join-Path $opsRoot 'remote-config.json') ([ordered]@{
    format_version = 1
    digest_algorithm = 'sha256(UTF-8 compact JSON of ordered remote_name/fetch_url_sanitized/push_url_sanitized/target_ref)'
    canonical_payload = $remoteConfigPayload
    sha256 = $remoteConfigDigest
})

$pushOperationId = [Guid]::NewGuid().ToString().ToLowerInvariant()
$lifecycleId = [Guid]::NewGuid().ToString().ToLowerInvariant()
$preflightQueryId = [Guid]::NewGuid().ToString().ToLowerInvariant()
$postflightQueryId = [Guid]::NewGuid().ToString().ToLowerInvariant()
$runId = [Guid]::NewGuid().ToString().ToLowerInvariant()
$lifecycleStarted = [DateTimeOffset]::UtcNow
$deadline = $lifecycleStarted.AddMinutes(10)
$deadlineText = $deadline.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)

$preflightStartedText = Get-UtcInstant
$preRepoPath = Join-Path $opsRoot 'github-repository-preflight.json'
$preBranchPath = Join-Path $opsRoot 'github-branch-preflight.json'
$preRepo = Get-ApiSnapshot $repoApi $preRepoPath
$preBranch = Get-ApiSnapshot ($repoApi + '/branches/main') $preBranchPath
$preRemoteOutput = @(git ls-remote origin $ref 2>&1)
$preRemoteExit = $LASTEXITCODE
$preflightCompletedText = Get-UtcInstant
$preRemoteLine = ($preRemoteOutput | Out-String).Trim()
$preRemoteSha = $null
if ($preRemoteExit -eq 0 -and $preRemoteLine) { $preRemoteSha = ($preRemoteLine -split '\s+')[0] }

$identityOkay = ($preRepo.id -eq 1421031011 -and $preRepo.full_name -eq 'LonelyTraderBay/maps-context-engine-4d' -and
    $preRepo.visibility -eq 'public' -and $preRepo.default_branch -eq 'main' -and $preRepo.archived -eq $false)
$branchOkay = ($preBranch.name -eq 'main' -and $preBranch.commit.sha -eq $base -and $preBranch.protected -eq $false)
$remoteOkay = ($preRemoteExit -eq 0 -and $preRemoteSha -eq $base)
if (-not ($identityOkay -and $branchOkay -and $remoteOkay)) {
    Write-JsonFile (Join-Path $opsRoot 'preflight-blocker.json') ([ordered]@{
        result = 'BLOCKED_BEFORE_PUSH'
        repository_identity_verified = $identityOkay
        branch_metadata_verified = $branchOkay
        local_remote_ref_verified = $remoteOkay
        observed_remote_sha = $preRemoteSha
        expected_base_sha = $base
        observed_at_utc = $preflightCompletedText
    })
    Write-Output 'BLOCKED_BEFORE_PUSH'
    exit 42
}

$repositoryIdentity = $plan.repository
$preflight = [ordered]@{
    query_id = $preflightQueryId
    lifecycle_id = $lifecycleId
    retry_index = 0
    candidate_sha = $candidate
    repository = $repositoryIdentity
    remote_config_digest = $remoteConfigDigest
    started_at_utc = $preflightStartedText
    completed_at_utc = $preflightCompletedText
    observed_remote_sha = $preRemoteSha
}
Write-JsonFile (Join-Path $opsRoot 'preflight.json') $preflight
$preflightProjection = [ordered]@{}
foreach ($entry in $preflight.GetEnumerator()) { $preflightProjection.Add($entry.Key, $entry.Value) }
$preflightProjection.Add('raw_api_ref', $opsRelative + '/preflight.json')

$currentFetchUrl = (git remote get-url origin).Trim()
$currentPushUrl = (git remote get-url --push origin).Trim()
if ($currentFetchUrl -ne $fetchUrl -or $currentPushUrl -ne $pushUrl) {
    Write-JsonFile (Join-Path $opsRoot 'preflight-blocker.json') ([ordered]@{
        result = 'BLOCKED_BEFORE_PUSH'
        reason = 'remote_config_changed_after_preflight'
        preflight_remote_config_sha256 = $remoteConfigDigest
        observed_at_utc = Get-UtcInstant
    })
    Write-Output 'BLOCKED_BEFORE_PUSH'
    exit 42
}
$pushStartedText = Get-UtcInstant
$targetRefspec = $candidate + ':' + $ref
$gitArgv = @('push', '--porcelain', 'origin', $targetRefspec)
$pushOutput = @(& git @gitArgv 2>&1)
$pushExit = $LASTEXITCODE
$pushCompletedText = Get-UtcInstant
$pushOutputText = ($pushOutput | Out-String -Width 4096)
[IO.File]::WriteAllText((Join-Path $opsRoot 'push-output.txt'), $pushOutputText, $utf8)

$postflightStartedText = Get-UtcInstant
$postRepoPath = Join-Path $opsRoot 'github-repository-postflight.json'
$postBranchPath = Join-Path $opsRoot 'github-branch-postflight.json'
$postRepo = Get-ApiSnapshot $repoApi $postRepoPath
$postBranch = Get-ApiSnapshot ($repoApi + '/branches/main') $postBranchPath
$postRemoteOutput = @(git ls-remote origin $ref 2>&1)
$postRemoteExit = $LASTEXITCODE
$postflightCompletedText = Get-UtcInstant
$postRemoteLine = ($postRemoteOutput | Out-String).Trim()
$postRemoteSha = $null
if ($postRemoteExit -eq 0 -and $postRemoteLine) { $postRemoteSha = ($postRemoteLine -split '\s+')[0] }

$postIdentityOkay = ($postRepo.id -eq 1421031011 -and $postRepo.full_name -eq 'LonelyTraderBay/maps-context-engine-4d' -and
    $postRepo.visibility -eq 'public' -and $postRepo.archived -eq $false -and
    $postBranch.name -eq 'main' -and $postBranch.commit.sha -eq $candidate -and $postRemoteExit -eq 0 -and
    $postRemoteSha -eq $candidate)
$postflight = [ordered]@{
    query_id = $postflightQueryId
    lifecycle_id = $lifecycleId
    retry_index = 0
    candidate_sha = $candidate
    repository = $repositoryIdentity
    remote_config_digest = $remoteConfigDigest
    started_at_utc = $postflightStartedText
    completed_at_utc = $postflightCompletedText
    observed_remote_sha = $postRemoteSha
}
Write-JsonFile (Join-Path $opsRoot 'postflight.json') $postflight
$postflightProjection = [ordered]@{}
foreach ($entry in $postflight.GetEnumerator()) { $postflightProjection.Add($entry.Key, $entry.Value) }
$postflightProjection.Add('raw_api_ref', $opsRelative + '/postflight.json')

$gitVersion = (git --version).Trim()
$gitExecutable = (Get-Command git).Source
$commandSourceRef = $opsRelative + '/push-driver.ps1'
$allArtifactRefs = @(
    $opsRelative + '/github-tool-repo-observation.json',
    $opsRelative + '/remote-config.json',
    $opsRelative + '/github-repository-preflight.json',
    $opsRelative + '/github-branch-preflight.json',
    $opsRelative + '/preflight.json',
    $opsRelative + '/push-output.txt',
    $opsRelative + '/github-repository-postflight.json',
    $opsRelative + '/github-branch-postflight.json',
    $opsRelative + '/postflight.json',
    $opsRelative + '/push-operation.json',
    $opsRelative + '/destination-observation.json',
    $opsRelative + '/command-lifecycle.json',
    $opsRelative + '/retention.json',
    $commandSourceRef
)
$pushOperation = [ordered]@{
    format_version = 1
    kind = 'push_operation'
    push_operation_id = $pushOperationId
    lifecycle_id = $lifecycleId
    retry_index = 0
    candidate_sha = $candidate
    credential_class = 'local_user'
    command_record_ref = $opsRelative + '/command-lifecycle.json'
    lifecycle_started_at_utc = $lifecycleStarted.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)
    deadline_at_utc = $deadlineText
    push_started_at_utc = $pushStartedText
    push_completed_at_utc = $pushCompletedText
    intervening_events = @()
    preflight = $preflightProjection
    postflight = $postflightProjection
}
$destinationObservation = [ordered]@{
    format_version = 1
    push_operation_id = $pushOperationId
    candidate_sha = $candidate
    repository = $repositoryIdentity
    local_remote_config_digest = $remoteConfigDigest
    observed_at_utc = $preflightCompletedText
    push_started_at_utc = $pushStartedText
    identity_verified = ($identityOkay -and $postIdentityOkay)
    visibility_verified = ($identityOkay -and $postIdentityOkay)
    operation_evidence_ref = $opsRelative + '/push-operation.json'
}
Write-JsonFile (Join-Path $opsRoot 'push-operation.json') $pushOperation
Write-JsonFile (Join-Path $opsRoot 'destination-observation.json') $destinationObservation

$finalStatus = if ($pushExit -eq 0) { 'SUCCEEDED' } else { 'FAILED' }
$commandState = if ($pushExit -eq 0) { 'SUCCEEDED' } else { 'FAILED' }
$commandContract = [ordered]@{
    command_id = 'T085-r4-payload-checkpoint-q-push-main-01'
    task_mode = 'implement'
    owner = 'Codex'
    baseline_ref = 'git:' + $base
    executable = 'git'
    argv = $gitArgv
    cwd = '.'
    tool_versions = [ordered]@{ git = $gitVersion; powershell = $PSVersionTable.PSVersion.ToString() }
    command_source_ref = $commandSourceRef
    input_refs = @('artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/revision-4/check-plan-canonical.json', 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/authority/task-start.txt', 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/revision-4/local-checks/payload/attempt-5/summary.json')
    contracts = @('S51', 'PUSH-FRESH-01', 'COMMAND-LIFECYCLE-01')
    fingerprints = [ordered]@{ check_plan_sha256 = (Get-FileHash -LiteralPath (Join-Path $repoRoot 'artifacts\git-delivery-support\T085-r1-33499f70-9889-4f8b-8776-9b46555825e5\revision-4\check-plan-canonical.json') -Algorithm SHA256).Hash.ToLowerInvariant(); remote_config_sha256 = $remoteConfigDigest; command_source_sha256 = $sourceHash }
    expected_writes = @($allArtifactRefs)
    temporary_roots = @()
    cache_roots = @()
    log_roots = @()
    download_roots = @()
    network_scope = @('api.github.com', 'github.com')
    child_processes = @($gitExecutable + ' ' + ($gitArgv -join ' '))
    side_effect_class = 'external_service'
    permission_ref = 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/authority/task-start.txt'
    storage_mode = 'bounded_small'
    volumes = @()
    budget_refs = @()
    retention_ref = $opsRelative + '/retention.json'
    deadline_at_utc = $deadlineText
    monitor_interval_ms = 1000
    run_id = $runId
    output_root = $opsRelative
    cleanup_plan_ref = $opsRelative + '/retention.json'
    cleanup_permission_ref = $null
}
Write-JsonFile (Join-Path $opsRoot 'retention.json') ([ordered]@{
    format_version = 1
    action = 'retain'
    reason = 'Payload push lifecycle evidence is required by S21 and must remain byte-stable for closing archive/readback.'
    retained_paths = @($allArtifactRefs + @($opsRelative + '/retention.json'))
})
$commandLifecycle = [ordered]@{
    format_version = 1
    kind = 'command_lifecycle'
    operation_id = $pushOperationId
    lifecycle_id = $lifecycleId
    retry_index = 0
    assignment_ref = $assignmentRef
    task_id = $taskId
    task_revision = $taskRevision
    delivery_id = $deliveryId
    command_state = $commandState
    contract = $commandContract
    result = [ordered]@{
        started_at_utc = $pushStartedText
        completed_at_utc = $postflightCompletedText
        command_exit_status = $pushExit
        termination_reason = 'completed'
        cleanup_status = 'NOT_REQUIRED'
        final_status = $finalStatus
        outputs_created = @($opsRelative + '/push-output.txt')
        artifact_refs = $allArtifactRefs
    }
}
Write-JsonFile (Join-Path $opsRoot 'command-lifecycle.json') $commandLifecycle

Write-Output (ConvertTo-Json -InputObject ([ordered]@{
    push_exit = $pushExit
    push_operation_id = $pushOperationId
    lifecycle_id = $lifecycleId
    preflight_remote_sha = $preRemoteSha
    postflight_remote_sha = $postRemoteSha
    identity_verified = $destinationObservation.identity_verified
    visibility_verified = $destinationObservation.visibility_verified
    evidence_root = $opsRelative
}) -Compress)
if ($pushExit -ne 0) { exit $pushExit }
if (-not $postIdentityOkay) { exit 43 }
