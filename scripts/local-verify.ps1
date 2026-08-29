[CmdletBinding()]
param(
    [ValidateSet("all", "0", "1", "2", "3", "4")]
    [string]$Tier = "all",
    [switch]$RemoteHelm,
    [string]$RemoteHost = "",
    [string]$RemoteUser = "deploy"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$script:AnyFail = $false

function Write-TierResult {
    param(
        [int]$Index,
        [string]$Name,
        [string]$State,
        [string]$Details
    )

    $prefix = switch ($State) {
        "PASS" { "[PASS]" }
        "FAIL" { "[FAIL]" }
        "SKIP" { "[SKIP]" }
        default { "[INFO]" }
    }

    Write-Host "$prefix TIER $Index - $Name - $Details"
}

function Get-FileHashes {
    param(
        [string]$Root
    )

    $map = @{}
    foreach ($file in Get-ChildItem -Path $Root -Recurse -File) {
        $rel = [System.IO.Path]::GetRelativePath($Root, $file.FullName)
        $hash = (Get-FileHash -Path $file.FullName -Algorithm SHA256).Hash
        $map[$rel] = $hash
    }
    return $map
}

function Compare-BundleDirectories {
    param(
        [string]$Actual,
        [string]$Expected
    )

    $actualMap = Get-FileHashes -Root $Actual
    $expectedMap = Get-FileHashes -Root $Expected
    $allKeys = @($actualMap.Keys + $expectedMap.Keys | Select-Object -Unique)
    $mismatches = @()

    foreach ($key in $allKeys) {
        if (-not $actualMap.ContainsKey($key)) {
            $mismatches += "$key (missing in generated bundle)"
            continue
        }
        if (-not $expectedMap.ContainsKey($key)) {
            $mismatches += "$key (missing in committed bundle)"
            continue
        }
        if ($actualMap[$key] -ne $expectedMap[$key]) {
            $mismatches += "$key (sha256 mismatch)"
        }
    }

    return $mismatches
}

function Get-SelectedTiers {
    param(
        [string]$SelectedTier,
        [bool]$RemoteCheckRequested
    )

    $wanted = @()
    if ($SelectedTier -eq "all") {
        $wanted = @(0, 1, 2, 3)
    }
    else {
        $wanted = @([int]$SelectedTier)
    }

    if ($RemoteCheckRequested) {
        $wanted += 4
    }

    return @($wanted | Sort-Object -Unique)
}

$selectedTiers = Get-SelectedTiers -SelectedTier $Tier -RemoteCheckRequested ([bool]$RemoteHelm)

if ($Tier -eq "4") {
    $selectedTiers = @(4)
}

if ($selectedTiers -contains 0) {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "SKIP" -Details "Go toolchain not installed; tier 0 cannot run."
    }
    else {
        Push-Location $repoRoot
        try {
            $gofmtDirs = & go list -f "{{.Dir}}" ./internal/... 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "FAIL" -Details (($gofmtDirs | Out-String).Trim())
                $script:AnyFail = $true
            }
            else {
                $gofmtOutput = & gofmt -l @($gofmtDirs | Where-Object { $_ -and $_.Trim() }) 2>&1
                $gofmtProblems = @($gofmtOutput | Where-Object { $_ -and $_.Trim().Length -gt 0 })
                if ($LASTEXITCODE -ne 0 -or $gofmtProblems.Count -gt 0) {
                    $detail = if ($gofmtProblems.Count -gt 0) { ($gofmtProblems | Out-String).Trim() } else { "gofmt reported unformatted files." }
                    Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "FAIL" -Details "gofmt failed: $detail"
                    $script:AnyFail = $true
                }
                else {
                    $buildOutput = & go build ./... 2>&1
                    if ($LASTEXITCODE -ne 0) {
                        Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (($buildOutput | Out-String).Trim())
                        $script:AnyFail = $true
                    }
                    else {
                        $vetOutput = & go vet ./... 2>&1
                        if ($LASTEXITCODE -ne 0) {
                            Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (($vetOutput | Out-String).Trim())
                            $script:AnyFail = $true
                        }
                        else {
                            $testOutput = & go test ./... -short -count=1 2>&1
                            if ($LASTEXITCODE -ne 0) {
                                Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (($testOutput | Out-String).Trim())
                                $script:AnyFail = $true
                            }
                            else {
                                Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "PASS" -Details "gofmt, build, vet, and short Go tests succeeded."
                            }
                        }
                    }
                }
            }
        }
        catch {
            Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "FAIL" -Details $_.Exception.Message
            $script:AnyFail = $true
        }
        finally {
            Pop-Location
        }
    }
}

if ($selectedTiers -contains 1) {
    $wsl = Get-Command wsl.exe -ErrorAction SilentlyContinue
    if (-not $wsl) {
        Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "SKIP" -Details "WSL not detected; POSIX deploy-script tests are not runnable on this host."
    }
    else {
        $tmpDir = Join-Path $repoRoot ".tmp"
        New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null
        $binaryPath = Join-Path $tmpDir "provisioning-linux.test"
        $savedGOOS = $env:GOOS
        $savedGOARCH = $env:GOARCH
        $savedCGO = $env:CGO_ENABLED
        try {
            $env:GOOS = "linux"
            $env:GOARCH = "amd64"
            $env:CGO_ENABLED = "0"

            $compileOutput = & go test -c -o $binaryPath ./internal/provisioning 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (($compileOutput | Out-String).Trim())
                $script:AnyFail = $true
            }
            else {
                $wslRepo = & wsl.exe wslpath -u $repoRoot 2>&1
                if ($LASTEXITCODE -ne 0) {
                    Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (($wslRepo | Out-String).Trim())
                    $script:AnyFail = $true
                }
                else {
                    $wslRepo = ($wslRepo | Out-String).Trim()
                    $runOutput = & wsl.exe bash -lc "cd '$wslRepo' && chmod +x .tmp/provisioning-linux.test && ./.tmp/provisioning-linux.test -test.v" 2>&1
                    if ($LASTEXITCODE -ne 0) {
                        Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (($runOutput | Out-String).Trim())
                        $script:AnyFail = $true
                    }
                    else {
                        Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "PASS" -Details "Linux cross-compile and POSIX deploy-script tests passed under WSL."
                    }
                }
            }
        }
        catch {
            Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details $_.Exception.Message
            $script:AnyFail = $true
        }
        finally {
            if (Test-Path $binaryPath) {
                Remove-Item -Path $binaryPath -Force
            }
            if ($null -ne $savedGOOS) { $env:GOOS = $savedGOOS } else { Remove-Item Env:GOOS -ErrorAction SilentlyContinue }
            if ($null -ne $savedGOARCH) { $env:GOARCH = $savedGOARCH } else { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue }
            if ($null -ne $savedCGO) { $env:CGO_ENABLED = $savedCGO } else { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue }
        }
    }
}

if ($selectedTiers -contains 2) {
    $seedPaths = @(
        "docs/instructor/overview.md",
        "docs/instructor/templates.md",
        "docs/instructor/os-recipes.md",
        "docs/instructor/playlists.md",
        "AGENTS.md",
        "docs/ai-prompts/build-workflow.md",
        "docs/ai-prompts/create-a-template.md",
        "docs/ai/build-workflow-prompt.md"
    )

    $tempBundleDir = Join-Path ([System.IO.Path]::GetTempPath()) ("wiki-bundle-" + [System.Guid]::NewGuid().ToString())
    New-Item -ItemType Directory -Path $tempBundleDir -Force | Out-Null

    $bundleArgs = @("run","./cmd/wiki-bundler","-repo-root",".","-out",$tempBundleDir)
    foreach ($seed in $seedPaths) {
        $bundleArgs += @("-seed", $seed)
    }

    Push-Location $repoRoot
    try {
        $bundleOutput = & go @bundleArgs 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-TierResult -Index 2 -Name "wiki bundle + byte-verify" -State "FAIL" -Details (($bundleOutput | Out-String).Trim())
            $script:AnyFail = $true
        }
        else {
            $expectedBundle = Join-Path $repoRoot "internal/docs/_bundle"
            $diffs = Compare-BundleDirectories -Actual $tempBundleDir -Expected $expectedBundle
            if ($diffs.Count -gt 0) {
                Write-TierResult -Index 2 -Name "wiki bundle + byte-verify" -State "FAIL" -Details (("Bundle mismatch: " + ($diffs | Select-Object -First 10) -join "; "))
                $script:AnyFail = $true
            }
            else {
                Write-TierResult -Index 2 -Name "wiki bundle + byte-verify" -State "PASS" -Details "Wiki bundle rebuilt and byte-verified against the checked-in bundle."
            }
        }
    }
    catch {
        Write-TierResult -Index 2 -Name "wiki bundle + byte-verify" -State "FAIL" -Details $_.Exception.Message
        $script:AnyFail = $true
    }
    finally {
        Remove-Item -Path $tempBundleDir -Recurse -Force -ErrorAction SilentlyContinue
        Pop-Location
    }
}

if ($selectedTiers -contains 3) {
    $docker = Get-Command docker -ErrorAction SilentlyContinue
    if (-not $docker) {
        Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "SKIP" -Details "Docker is not installed, so the integration tier cannot run here."
    }
    else {
        Push-Location $repoRoot
        try {
            $composeOutput = & docker compose -f docker-compose.dev.yaml up -d --wait 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details (($composeOutput | Out-String).Trim())
                $script:AnyFail = $true
            }
            else {
                $hasIntegrationBuild = $false
                foreach ($file in Get-ChildItem -Path $repoRoot -Recurse -Filter *.go) {
                    $content = Get-Content -Path $file.FullName -Raw -ErrorAction SilentlyContinue
                    if ($content -match '(?m)//go:build integration|// \+build integration') {
                        $hasIntegrationBuild = $true
                        break
                    }
                }

                if (-not $hasIntegrationBuild) {
                    Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "PASS" -Details "Docker stack started successfully; no integration-tagged tests are present in this repo."
                }
                else {
                    $testOutput = & go test -tags=integration ./... 2>&1
                    if ($LASTEXITCODE -ne 0) {
                        Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details (($testOutput | Out-String).Trim())
                        $script:AnyFail = $true
                    }
                    else {
                        Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "PASS" -Details "Docker stack started and integration-tagged tests passed."
                    }
                }
            }
        }
        catch {
            Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details $_.Exception.Message
            $script:AnyFail = $true
        }
        finally {
            $downOutput = & docker compose -f docker-compose.dev.yaml down -v 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "SKIP" -Details ("Docker cleanup reported a warning: " + (($downOutput | Out-String).Trim()))
            }
            Pop-Location
        }
    }
}

if ($selectedTiers -contains 4) {
    if (-not $RemoteHelm) {
        Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "SKIP" -Details "Tier 4 is opt-in: pass -RemoteHelm to render/lint the production chart remotely without mutation."
    }
    elseif ([string]::IsNullOrWhiteSpace($RemoteHost)) {
        Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "FAIL" -Details "-RemoteHelm requires -RemoteHost (for example, user@k3sv01.lab.jmal.io)."
        $script:AnyFail = $true
    }
    else {
        $ssh = Get-Command ssh -ErrorAction SilentlyContinue
        if (-not $ssh) {
            Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "SKIP" -Details "SSH client is not installed, so tier 4 cannot run here."
        }
        else {
            $remoteTarget = "$RemoteUser@$RemoteHost"
            $remoteScript = @"
set -euo pipefail
cd /opt/selfservice 2>/dev/null || cd /srv/selfservice 2>/dev/null || cd /home/$RemoteUser/selfservice
helm lint deploy/helm/selfservice -f deploy/helm/selfservice/values.yaml
helm lint deploy/helm/selfservice -f deploy/helm/selfservice/values.yaml -f deploy/helm/selfservice/values.prod.yaml
helm template selfservice deploy/helm/selfservice -f deploy/helm/selfservice/values.yaml -f deploy/helm/selfservice/values.prod.yaml > /tmp/rendered-selfservice-prod.yaml
grep -q 'SYNTHETIC_LIFECYCLE_ENABLED' /tmp/rendered-selfservice-prod.yaml
"@
            $remoteOutput = & ssh $remoteTarget $remoteScript 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "FAIL" -Details (($remoteOutput | Out-String).Trim())
                $script:AnyFail = $true
            }
            else {
                Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "PASS" -Details "Remote helm lint + template checks succeeded without mutating production."
            }
        }
    }
}

if ($script:AnyFail) {
    Write-Host "OVERALL: FAIL - at least one runnable tier failed."
    exit 1
}

$skipped = 0
foreach ($tier in $selectedTiers) {
    # Result is only tracked via stdout, so use the current set of tier invocations,
    # which is sufficient for a final summary.
    $skipped += 0
}

Write-Host "OVERALL: PASS - all selected tiers succeeded or were intentionally skipped."
exit 0
