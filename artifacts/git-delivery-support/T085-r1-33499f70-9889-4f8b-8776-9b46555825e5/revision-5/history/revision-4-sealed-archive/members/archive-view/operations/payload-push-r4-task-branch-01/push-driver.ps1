$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repoRoot = (Resolve-Path -LiteralPath '.').Path
$supportRoot = Join-Path $repoRoot 'artifacts\git-delivery-support\T085-r1-33499f70-9889-4f8b-8776-9b46555825e5'
$opsRelative = 'archive-view/operations/payload-push-r4-task-branch-01'
$opsRoot = Join-Path $supportRoot ($opsRelative -replace '/', '\')
$expectedBase = '5c5daf8d52dc8b838e678deff04d0a773e994ca5'
$candidate = 'acfdb8088516fac1cb4e190e2c6c1f5b3bddbba5'
$branchName = 'codex/t003-d089-regressions'
$targetRef = 'refs/heads/' + $branchName
$repoApi = 'https://api.github.com/repos/LonelyTraderBay/maps-context-engine-4d'
$headers = @{ 'User-Agent' = 'MCE4D-T085-AW08'; 'Accept' = 'application/vnd.github+json'; 'Cache-Control' = 'no-cache'; 'X-GitHub-Api-Version' = '2022-11-28' }
$utf8 = [System.Text.UTF8Encoding]::new($false)
if ((git rev-parse HEAD).Trim() -ne $candidate) { throw 'candidate HEAD mismatch' }
if ((git branch --show-current).Trim() -ne 'codex/t085-r1-delivery') { throw 'delivery branch mismatch' }
git diff --quiet HEAD --
if ($LASTEXITCODE -ne 0) { throw 'tracked working tree dirty' }
$repoUrl = (git remote get-url origin).Trim()
if ($repoUrl -ne 'https://github.com/LonelyTraderBay/maps-context-engine-4d.git') { throw 'remote URL mismatch' }
if (-not (Test-Path -LiteralPath $opsRoot)) { New-Item -ItemType Directory -Path $opsRoot | Out-Null }
$preTime = [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)
$preRepo = Invoke-RestMethod -Uri ($repoApi + '?codex_observe=' + [Guid]::NewGuid().ToString('N')) -Headers $headers -TimeoutSec 20
$encodedBranch = [Uri]::EscapeDataString($branchName)
$preBranch = Invoke-RestMethod -Uri ($repoApi + '/branches/' + $encodedBranch + '?codex_observe=' + [Guid]::NewGuid().ToString('N')) -Headers $headers -TimeoutSec 20
$preRemote = @(git ls-remote origin $targetRef)
if ($LASTEXITCODE -ne 0) { throw 'preflight ls-remote failed' }
$preRemoteSha = ($preRemote | Select-Object -First 1) -split '\s+' | Select-Object -First 1
if ($preRepo.id -ne 1421031011 -or $preRepo.full_name -ne 'LonelyTraderBay/maps-context-engine-4d' -or $preRepo.visibility -ne 'public' -or $preBranch.name -ne $branchName -or $preBranch.commit.sha -ne $expectedBase -or $preRemoteSha -ne $expectedBase) { throw "task branch preflight mismatch: $preRemoteSha / $($preBranch.commit.sha)" }
[IO.File]::WriteAllText((Join-Path $opsRoot 'github-repository-preflight.json'), (ConvertTo-Json -InputObject $preRepo -Depth 50) + [Environment]::NewLine, $utf8)
[IO.File]::WriteAllText((Join-Path $opsRoot 'github-branch-preflight.json'), (ConvertTo-Json -InputObject $preBranch -Depth 50) + [Environment]::NewLine, $utf8)
$pushStart = [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)
$argv = @('push','--porcelain','origin',($candidate + ':' + $targetRef))
$out = @(& git @argv 2>&1)
$pushExit = $LASTEXITCODE
$pushEnd = [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)
[IO.File]::WriteAllText((Join-Path $opsRoot 'push-output.txt'), ($out | Out-String -Width 4096), $utf8)
$postRepo = Invoke-RestMethod -Uri ($repoApi + '?codex_observe=' + [Guid]::NewGuid().ToString('N')) -Headers $headers -TimeoutSec 20
$postBranch = Invoke-RestMethod -Uri ($repoApi + '/branches/' + $encodedBranch + '?codex_observe=' + [Guid]::NewGuid().ToString('N')) -Headers $headers -TimeoutSec 20
$postRemote = @(git ls-remote origin $targetRef)
if ($LASTEXITCODE -ne 0) { throw 'postflight ls-remote failed' }
$postRemoteSha = ($postRemote | Select-Object -First 1) -split '\s+' | Select-Object -First 1
$postTime = [DateTimeOffset]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss.fffffff'Z'", [Globalization.CultureInfo]::InvariantCulture)
[IO.File]::WriteAllText((Join-Path $opsRoot 'github-repository-postflight.json'), (ConvertTo-Json -InputObject $postRepo -Depth 50) + [Environment]::NewLine, $utf8)
[IO.File]::WriteAllText((Join-Path $opsRoot 'github-branch-postflight.json'), (ConvertTo-Json -InputObject $postBranch -Depth 50) + [Environment]::NewLine, $utf8)
$observation = [ordered]@{ format_version=1; task_id='T085'; task_revision=4; delivery_id='711606c8-9642-44fc-905a-7ebfd8cfd686'; branch_ref=$targetRef; candidate_sha=$candidate; preflight_remote_sha=$preRemoteSha; postflight_remote_sha=$postRemoteSha; github_preflight_sha=$preBranch.commit.sha; github_postflight_sha=$postBranch.commit.sha; push_argv=@('git')+$argv; push_started_at_utc=$pushStart; push_completed_at_utc=$pushEnd; push_exit=$pushExit; observed_at_utc=$postTime; identity_verified=($postRepo.id -eq 1421031011 -and $postRepo.full_name -eq 'LonelyTraderBay/maps-context-engine-4d'); visibility_verified=($postRepo.visibility -eq 'public'); result= $(if($pushExit -eq 0 -and $postRemoteSha -eq $candidate -and $postBranch.commit.sha -eq $candidate){'PASS'}else{'FAIL'}) }
[IO.File]::WriteAllText((Join-Path $opsRoot 'task-branch-push-observation.json'), (ConvertTo-Json -InputObject $observation -Depth 20) + [Environment]::NewLine, $utf8)
$paths = @('github-repository-preflight.json','github-branch-preflight.json','github-repository-postflight.json','github-branch-postflight.json','push-output.txt','task-branch-push-observation.json','retention.json','push-driver.ps1')
$retain = [ordered]@{format_version=1;action='retain';reason='T085 revision-4 task-branch mirror push and exact ref observation for closing archive/readback.';retained_paths=@((@($paths | ForEach-Object { $opsRelative + '/' + $_ }) -join ' '),($opsRelative + '/retention.json'))}
[IO.File]::WriteAllText((Join-Path $opsRoot 'retention.json'), (ConvertTo-Json -InputObject $retain -Depth 10) + [Environment]::NewLine, $utf8)
$sourcePath = Join-Path $PSScriptRoot 'push_driver_revision4_task_branch.ps1'
Copy-Item -LiteralPath $sourcePath -Destination (Join-Path $opsRoot 'push-driver.ps1') -Force
Write-Output (ConvertTo-Json -InputObject $observation -Compress)
if ($observation.result -ne 'PASS') { exit 44 }
