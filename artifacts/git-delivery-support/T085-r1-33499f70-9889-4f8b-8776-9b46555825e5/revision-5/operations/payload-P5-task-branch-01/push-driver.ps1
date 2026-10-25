param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('main', 'codex/t003-d089-regressions')]
    [string]$TargetBranch,
    [Parameter(Mandatory = $true)]
    [string]$PermissionSnapshotPath
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$candidate = '3ea6c38d67772b26ffce5ad4a8661772aa806cdc'
$base = 'c04441ab75baac426492b8ad5be9167dcb1b0f3c'
$taskId = 'T085'
$taskRevision = 5
$deliveryId = 'f2f6cdbc-13f0-493d-9f83-d2992aded582'
$assignmentRef = 'S53/AW-01/AW-08:user-assignment-2026-10-15'
$repoId = 1421031011
$repoFullName = 'LonelyTraderBay/maps-context-engine-4d'
$repoApi = 'https://api.github.com/repos/LonelyTraderBay/maps-context-engine-4d'
$expectedRemote = 'https://github.com/LonelyTraderBay/maps-context-engine-4d.git'
$branchName = $TargetBranch
$ref = 'refs/heads/' + $branchName
$operationSlug = if ($branchName -eq 'main') { 'payload-P5-main-01' } else { 'payload-P5-task-branch-01' }
$repoRoot = (Resolve-Path -LiteralPath '.').Path
$supportRoot = Join-Path $repoRoot 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5'
$revisionRoot = Join-Path $supportRoot 'revision-5'
$opsRelative = 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/revision-5/operations/' + $operationSlug
$opsRoot = Join-Path $repoRoot ($opsRelative -replace '/', '\')
$utf8 = [System.Text.UTF8Encoding]::new($false)
$headers = @{ 'User-Agent' = 'MCE4D-T085-AW08'; 'Accept' = 'application/vnd.github+json'; 'X-GitHub-Api-Version' = '2022-11-28' }

function Get-UtcInstant {
    [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)
}
function Write-JsonFile([string]$Path, [object]$Value) {
    $json = ConvertTo-Json -InputObject $Value -Depth 100
    [IO.File]::WriteAllText($Path, $json + [Environment]::NewLine, $utf8)
}
function Assert-Contained([string]$Path, [string]$Root) {
    $full = [IO.Path]::GetFullPath($Path)
    $prefix = [IO.Path]::GetFullPath($Root).TrimEnd('\') + '\'
    if (-not $full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'Output path escaped its authorized root.' }
}
function Ensure-PlainDirectory([string]$Path, [string]$Root) {
    Assert-Contained $Path $Root
    if (-not (Test-Path -LiteralPath $Path)) { New-Item -ItemType Directory -Path $Path | Out-Null }
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Output directory is a reparse point.' }
}
function Get-ApiSnapshot([string]$Uri, [string]$Path) {
    Invoke-WebRequest -Uri $Uri -Headers $headers -TimeoutSec 15 -OutFile $Path | Out-Null
    Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -AsHashtable
}

if ([IO.Path]::GetFullPath((git rev-parse --show-toplevel).Trim().Replace('/', '\')) -ne [IO.Path]::GetFullPath($repoRoot)) { throw 'Git root mismatch.' }
if ((git rev-parse HEAD).Trim() -ne $candidate) { throw 'Payload P5 candidate HEAD mismatch.' }
if ((git branch --show-current).Trim() -ne 'codex/t085-r1-delivery') { throw 'Delivery branch mismatch.' }
git diff --quiet HEAD --
if ($LASTEXITCODE -ne 0) { throw 'Tracked working tree is not clean.' }
git diff --cached --quiet
if ($LASTEXITCODE -ne 0) { throw 'Index is not clean.' }
git merge-base --is-ancestor $base $candidate
if ($LASTEXITCODE -ne 0) { throw 'Candidate is not a descendant of the recorded base.' }

$permission = Get-Content -LiteralPath $PermissionSnapshotPath -Raw | ConvertFrom-Json -AsHashtable -DateKind String
if ($permission.repository_id -ne $repoId -or $permission.repository_full_name -ne $repoFullName -or
    $permission.visibility -ne 'public' -or $permission.default_branch -ne 'main' -or $permission.permissions.push -ne $true) {
    throw 'Connected GitHub repository identity or push permission did not match.'
}
$permissionObserved = [DateTimeOffset]::Parse($permission.observed_at_utc, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::RoundtripKind)
if (([DateTimeOffset]::UtcNow - $permissionObserved).TotalSeconds -gt 30) { throw 'Connected-repository permission snapshot exceeded the 30 second preflight window.' }

$fetchUrl = (git remote get-url origin).Trim()
$pushUrl = (git remote get-url --push origin).Trim()
if ($fetchUrl -ne $expectedRemote -or $pushUrl -ne $expectedRemote) { throw 'Origin differs from the verified public repository URL.' }
$repoRootPrefix = [IO.Path]::GetFullPath($repoRoot).TrimEnd('\') + '\'
$sourceScript = [IO.Path]::GetFullPath($MyInvocation.MyCommand.Path)
Assert-Contained $sourceScript (Join-Path $repoRoot 'work/T085-r1-implementation')
Ensure-PlainDirectory $opsRoot $revisionRoot
$opsPrefix = [IO.Path]::GetFullPath($opsRoot).TrimEnd('\') + '\'
if (-not $opsPrefix.StartsWith([IO.Path]::GetFullPath($revisionRoot).TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Operation root escaped revision 5.' }
$commandSourceRef = $opsRelative + '/push-driver.ps1'
$sourceHash = (Get-FileHash -LiteralPath $sourceScript -Algorithm SHA256).Hash.ToLowerInvariant()
$retainedScript = Join-Path $opsRoot 'push-driver.ps1'
Copy-Item -LiteralPath $sourceScript -Destination $retainedScript
if ((Get-FileHash -LiteralPath $retainedScript -Algorithm SHA256).Hash.ToLowerInvariant() -ne $sourceHash) { throw 'Retained driver bytes differ from executed source.' }
$permissionRef = $opsRelative + '/github-tool-repo-observation.json'
Copy-Item -LiteralPath $PermissionSnapshotPath -Destination (Join-Path $opsRoot 'github-tool-repo-observation.json')

$remoteConfig = [ordered]@{ remote_name = 'origin'; fetch_url_sanitized = $fetchUrl; push_url_sanitized = $pushUrl; target_ref = $ref }
$remoteConfigPayload = ConvertTo-Json -InputObject $remoteConfig -Compress
$remoteConfigDigest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($utf8.GetBytes($remoteConfigPayload))).ToLowerInvariant()
$remoteConfigRef = $opsRelative + '/remote-config.json'
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
$branchApi = $repoApi + '/branches/' + [Uri]::EscapeDataString($branchName)

$preflightStartedText = Get-UtcInstant
$preRepoPath = Join-Path $opsRoot 'github-repository-preflight.json'
$preBranchPath = Join-Path $opsRoot 'github-branch-preflight.json'
$preRepo = Get-ApiSnapshot $repoApi $preRepoPath
$preBranch = Get-ApiSnapshot $branchApi $preBranchPath
$preRemoteOutput = @(git ls-remote origin $ref 2>&1)
$preRemoteExit = $LASTEXITCODE
$preflightCompletedText = Get-UtcInstant
$preRemoteLine = ($preRemoteOutput | Out-String).Trim()
$preRemoteSha = $null
if ($preRemoteExit -eq 0 -and $preRemoteLine) { $preRemoteSha = ($preRemoteLine -split '\s+')[0] }
$identityOkay = ($preRepo.id -eq $repoId -and $preRepo.full_name -eq $repoFullName -and $preRepo.visibility -eq 'public' -and $preRepo.default_branch -eq 'main' -and $preRepo.archived -eq $false)
$branchOkay = ($preBranch.name -eq $branchName -and $preBranch.commit.sha -eq $base -and $preBranch.protected -eq $false)
$remoteOkay = ($preRemoteExit -eq 0 -and $preRemoteSha -eq $base)
if (-not ($identityOkay -and $branchOkay -and $remoteOkay)) { throw "Push preflight failed closed: repo=$identityOkay branch=$branchOkay remote=$remoteOkay sha=$preRemoteSha" }
$repositoryIdentity = [ordered]@{
    host = 'github.com'; owner = 'LonelyTraderBay'; name = 'maps-context-engine-4d'; repository_id = $repoId
    visibility = 'public'; ref = $ref; remote_name = 'origin'; remote_url_sanitized = $expectedRemote
    publication_authority_ref = 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/authority/task-start.txt'
}
$preflight = [ordered]@{
    query_id = $preflightQueryId; lifecycle_id = $lifecycleId; retry_index = 0; candidate_sha = $candidate
    repository = $repositoryIdentity; remote_config_digest = $remoteConfigDigest
    started_at_utc = $preflightStartedText; completed_at_utc = $preflightCompletedText
    observed_remote_sha = $preRemoteSha
}
Write-JsonFile (Join-Path $opsRoot 'preflight.json') $preflight

$pushStartedText = Get-UtcInstant
$gitArgv = @('push', '--porcelain', 'origin', ($candidate + ':' + $ref))
$pushOutput = @(& git @gitArgv 2>&1)
$pushExit = $LASTEXITCODE
$pushCompletedText = Get-UtcInstant
$pushOutputText = ($pushOutput | Out-String -Width 4096)
[IO.File]::WriteAllText((Join-Path $opsRoot 'push-output.txt'), $pushOutputText, $utf8)

$postflightStartedText = Get-UtcInstant
$postRepoPath = Join-Path $opsRoot 'github-repository-postflight.json'
$postBranchPath = Join-Path $opsRoot 'github-branch-postflight.json'
$postRepo = Get-ApiSnapshot $repoApi $postRepoPath
$postBranch = Get-ApiSnapshot $branchApi $postBranchPath
$postRemoteOutput = @(git ls-remote origin $ref 2>&1)
$postRemoteExit = $LASTEXITCODE
$postflightCompletedText = Get-UtcInstant
$postRemoteLine = ($postRemoteOutput | Out-String).Trim()
$postRemoteSha = $null
if ($postRemoteExit -eq 0 -and $postRemoteLine) { $postRemoteSha = ($postRemoteLine -split '\s+')[0] }
$postIdentityOkay = ($postRepo.id -eq $repoId -and $postRepo.full_name -eq $repoFullName -and $postRepo.visibility -eq 'public' -and $postRepo.archived -eq $false -and $postBranch.name -eq $branchName -and $postBranch.commit.sha -eq $candidate -and $postRemoteExit -eq 0 -and $postRemoteSha -eq $candidate)
$postflight = [ordered]@{
    query_id = $postflightQueryId; lifecycle_id = $lifecycleId; retry_index = 0; candidate_sha = $candidate
    repository = $repositoryIdentity; remote_config_digest = $remoteConfigDigest
    started_at_utc = $postflightStartedText; completed_at_utc = $postflightCompletedText
    observed_remote_sha = $postRemoteSha
}
Write-JsonFile (Join-Path $opsRoot 'postflight.json') $postflight
$preflightProjection = [ordered]@{}; foreach ($entry in $preflight.GetEnumerator()) { $preflightProjection.Add($entry.Key, $entry.Value) }; $preflightProjection.Add('raw_api_ref', $opsRelative + '/preflight.json')
$postflightProjection = [ordered]@{}; foreach ($entry in $postflight.GetEnumerator()) { $postflightProjection.Add($entry.Key, $entry.Value) }; $postflightProjection.Add('raw_api_ref', $opsRelative + '/postflight.json')

$artifactRefs = @(
    $permissionRef, $remoteConfigRef, $opsRelative + '/github-repository-preflight.json', $opsRelative + '/github-branch-preflight.json',
    $opsRelative + '/preflight.json', $opsRelative + '/push-output.txt', $opsRelative + '/github-repository-postflight.json',
    $opsRelative + '/github-branch-postflight.json', $opsRelative + '/postflight.json', $opsRelative + '/push-operation.json',
    $opsRelative + '/destination-observation.json', $opsRelative + '/command-lifecycle.json', $opsRelative + '/retention.json', $commandSourceRef
)
$pushOperation = [ordered]@{
    format_version = 1; kind = 'push_operation'; push_operation_id = $pushOperationId; lifecycle_id = $lifecycleId
    retry_index = 0; candidate_sha = $candidate; credential_class = 'local_user'
    command_record_ref = $opsRelative + '/command-lifecycle.json'
    lifecycle_started_at_utc = $lifecycleStarted.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)
    deadline_at_utc = $deadlineText; push_started_at_utc = $pushStartedText; push_completed_at_utc = $pushCompletedText
    intervening_events = @(); preflight = $preflightProjection; postflight = $postflightProjection
}
$destinationObservation = [ordered]@{
    format_version = 1; push_operation_id = $pushOperationId; candidate_sha = $candidate
    repository = $repositoryIdentity; local_remote_config_digest = $remoteConfigDigest
    observed_at_utc = $preflightCompletedText; push_started_at_utc = $pushStartedText
    identity_verified = ($identityOkay -and $postIdentityOkay); visibility_verified = ($identityOkay -and $postIdentityOkay)
    operation_evidence_ref = $opsRelative + '/push-operation.json'
}
Write-JsonFile (Join-Path $opsRoot 'push-operation.json') $pushOperation
Write-JsonFile (Join-Path $opsRoot 'destination-observation.json') $destinationObservation
$finalStatus = if ($pushExit -eq 0 -and $postIdentityOkay) { 'SUCCEEDED' } else { 'FAILED' }
$commandContract = [ordered]@{
    command_id = 'T085-r5-payload-push-' + ($branchName -replace '/', '-')
    task_mode = 'implement'; owner = 'Codex'; baseline_ref = 'git:' + $base; executable = 'git'; argv = $gitArgv; cwd = '.'
    tool_versions = [ordered]@{ git = (git --version).Trim(); powershell = $PSVersionTable.PSVersion.ToString() }
    command_source_ref = $commandSourceRef
    input_refs = @('tools/ci/check-plan.json', 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/revision-5/task-mode.json', 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/revision-5/local-checks/payload/attempt-1/summary.json')
    contracts = @('S51', 'PUSH-FRESH-01', 'COMMAND-LIFECYCLE-01')
    fingerprints = [ordered]@{ remote_config_sha256 = $remoteConfigDigest; command_source_sha256 = $sourceHash }
    expected_writes = $artifactRefs; temporary_roots = @(); cache_roots = @(); log_roots = @(); download_roots = @()
    network_scope = @('api.github.com', 'github.com'); child_processes = @((Get-Command git).Source + ' ' + ($gitArgv -join ' '))
    side_effect_class = 'external_service'; permission_ref = 'artifacts/git-delivery-support/T085-r1-33499f70-9889-4f8b-8776-9b46555825e5/authority/task-start.txt'
    storage_mode = 'bounded_small'; volumes = @(); budget_refs = @(); retention_ref = $opsRelative + '/retention.json'
    deadline_at_utc = $deadlineText; monitor_interval_ms = 1000; run_id = $runId; output_root = $opsRelative
    cleanup_plan_ref = $opsRelative + '/retention.json'; cleanup_permission_ref = $null
}
Write-JsonFile (Join-Path $opsRoot 'retention.json') ([ordered]@{ format_version = 1; action = 'retain'; reason = 'S21 and the closing archive require immutable payload P5 push lifecycle evidence.'; retained_paths = @($artifactRefs + @($opsRelative + '/retention.json')) })
Write-JsonFile (Join-Path $opsRoot 'command-lifecycle.json') ([ordered]@{
    format_version = 1; kind = 'command_lifecycle'; operation_id = $pushOperationId; lifecycle_id = $lifecycleId
    retry_index = 0; assignment_ref = $assignmentRef; task_id = $taskId; task_revision = $taskRevision; delivery_id = $deliveryId
    command_state = $finalStatus; contract = $commandContract
    result = [ordered]@{ started_at_utc = $pushStartedText; completed_at_utc = $postflightCompletedText; command_exit_status = $pushExit; termination_reason = 'completed'; cleanup_status = 'NOT_REQUIRED'; final_status = $finalStatus; outputs_created = @($opsRelative + '/push-output.txt'); artifact_refs = $artifactRefs }
})

Write-Output (ConvertTo-Json -InputObject ([ordered]@{ push_exit = $pushExit; postflight_verified = $postIdentityOkay; push_operation_id = $pushOperationId; lifecycle_id = $lifecycleId; preflight_remote_sha = $preRemoteSha; postflight_remote_sha = $postRemoteSha; evidence_root = $opsRelative }) -Compress)
if ($pushExit -ne 0) { exit $pushExit }
if (-not $postIdentityOkay) { exit 43 }
