[CmdletBinding()]
param(
    [ValidateSet("all", "0", "1", "2", "3", "4")]
    [string]$Tier = "all",
    [switch]$RemoteHelm,
    [string]$RemoteHost = "",
    [string]$RemoteUser = "jmal"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$script:AnyFail = $false
$script:ExpectedWikiSeeds = @(
    "docs/instructor/overview.md",
    "docs/instructor/templates.md",
    "docs/instructor/os-recipes.md",
    "docs/instructor/playlists.md",
    "AGENTS.md",
    "docs/ai-prompts/build-workflow.md",
    "docs/ai-prompts/create-a-template.md",
    "docs/ai/build-workflow-prompt.md"
)

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

function Get-WikiSeedPaths {
    param(
        [string]$Root
    )

    $makefilePath = Join-Path $Root "Makefile"
    if (-not (Test-Path $makefilePath)) {
        throw "Makefile not found at $makefilePath"
    }

    $collect = $false
    $seeds = @()
    foreach ($line in Get-Content -Path $makefilePath) {
        if (-not $collect) {
            if ($line -match '^\s*WIKI_SEEDS\s*:=\s*\\\s*$') {
                $collect = $true
            }
            continue
        }

        if ($line -match '^\s*WIKI_OUT\s*:=' ) {
            break
        }

        $trimmed = $line.Trim()
        if ([string]::IsNullOrWhiteSpace($trimmed)) {
            continue
        }

        foreach ($chunk in ($trimmed -split '\\')) {
            $seed = $chunk.Trim()
            if (-not [string]::IsNullOrWhiteSpace($seed)) {
                $seeds += $seed
            }
        }
    }

    if ($seeds.Count -eq 0) {
        throw "Could not parse WIKI_SEEDS from $makefilePath"
    }

    return $seeds
}

function Assert-WikiSeedSetMatchesMakefile {
    param(
        [string[]]$SeedPaths,
        [string[]]$ExpectedSeeds
    )

    if ($SeedPaths.Count -ne $ExpectedSeeds.Count) {
        throw "WIKI_SEEDS drifted: Makefile defines $($SeedPaths.Count) seeds but the local verifier expects $($ExpectedSeeds.Count)."
    }

    $normalizedActual = @($SeedPaths | ForEach-Object { $_.Trim() } | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Sort-Object -Unique)
    $normalizedExpected = @($ExpectedSeeds | ForEach-Object { $_.Trim() } | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Sort-Object -Unique)

    $missing = @($normalizedExpected | Where-Object { $normalizedActual -notcontains $_ })
    if ($missing.Count -gt 0) {
        throw "WIKI_SEEDS drifted: missing expected seed(s): $($missing -join ', ')"
    }

    $extra = @($normalizedActual | Where-Object { $normalizedExpected -notcontains $_ })
    if ($extra.Count -gt 0) {
        throw "WIKI_SEEDS drifted: unexpected seed(s): $($extra -join ', ')"
    }
}

function Resolve-RemoteTarget {
    param(
        [string]$HostValue,
        [string]$UserValue
    )

    $hostText = ($HostValue ?? "").Trim()
    if ([string]::IsNullOrWhiteSpace($hostText)) {
        return @{ User = $UserValue; Host = ""; Target = "" }
    }

    $parts = $hostText -split '@', 2
    if ($parts.Count -gt 1 -and -not [string]::IsNullOrWhiteSpace($parts[0]) -and -not [string]::IsNullOrWhiteSpace($parts[1])) {
        return @{ User = $parts[0].Trim(); Host = $parts[1].Trim(); Target = ($parts[0].Trim() + "@" + $parts[1].Trim()) }
    }

    return @{ User = $UserValue; Host = $hostText; Target = ($UserValue + "@" + $hostText) }
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

    if ($RemoteCheckRequested -or $SelectedTier -eq "4") {
        $wanted += 4
    }

    return @($wanted | Sort-Object -Unique)
}

$selectedTiers = Get-SelectedTiers -SelectedTier $Tier -RemoteCheckRequested ([bool]$RemoteHelm)
if ($Tier -eq "4") {
    $selectedTiers = @(4)
}

if ($selectedTiers -contains 0) {
    $explicit = ($Tier -ne "all")
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        if ($explicit) {
            Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "FAIL" -Details "Go toolchain is required for the explicit tier 0 check but is not installed on PATH."
            $script:AnyFail = $true
        }
        else {
            Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "SKIP" -Details "Go toolchain not installed; auto-detected tier 0 is skipped."
        }
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
    $explicit = ($Tier -ne "all")
    $goCmd = Get-Command go -ErrorAction SilentlyContinue
    $wslCmd = Get-Command wsl.exe -ErrorAction SilentlyContinue

    if (-not $goCmd -or -not $wslCmd) {
        if ($explicit) {
            $missing = @()
            if (-not $goCmd) { $missing += "Go toolchain" }
            if (-not $wslCmd) { $missing += "WSL" }
            $detail = ($missing -join " and ") + " is required for the explicit tier 1 check but is not installed on PATH."
            Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details $detail
            $script:AnyFail = $true
        }
        else {
            $missing = @()
            if (-not $goCmd) { $missing += "Go toolchain" }
            if (-not $wslCmd) { $missing += "WSL" }
            $detail = ($missing -join " and ") + " not detected; auto-detected tier 1 is skipped."
            Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "SKIP" -Details $detail
        }
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
    $explicit = ($Tier -ne "all")
    $goCmd = Get-Command go -ErrorAction SilentlyContinue
    if (-not $goCmd) {
        if ($explicit) {
            Write-TierResult -Index 2 -Name "wiki bundle + byte-verify" -State "FAIL" -Details "Go toolchain is required for the explicit tier 2 check but is not installed on PATH."
            $script:AnyFail = $true
        }
        else {
            Write-TierResult -Index 2 -Name "wiki bundle + byte-verify" -State "SKIP" -Details "Go toolchain not installed; auto-detected tier 2 is skipped."
        }
    }
    else {
        $seedPaths = Get-WikiSeedPaths -Root $repoRoot
        Assert-WikiSeedSetMatchesMakefile -SeedPaths $seedPaths -ExpectedSeeds $script:ExpectedWikiSeeds

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
}

if ($selectedTiers -contains 3) {
    $explicit = ($Tier -ne "all")
    $docker = Get-Command docker -ErrorAction SilentlyContinue
    if (-not $docker) {
        if ($explicit) {
            Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details "Docker is required for the explicit tier 3 check but is not installed on PATH."
            $script:AnyFail = $true
        }
        else {
            Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "SKIP" -Details "Docker is not installed; auto-detected tier 3 is skipped."
        }
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
                Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details ("Docker cleanup failed: " + (($downOutput | Out-String).Trim()))
                $script:AnyFail = $true
            }
            Pop-Location
        }
    }
}

if ($selectedTiers -contains 4) {
    $remoteRequested = $RemoteHelm -or ($Tier -eq "4")
    if (-not $remoteRequested) {
        Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "SKIP" -Details "Tier 4 is opt-in: pass -RemoteHelm to render/lint the production chart remotely without mutation."
    }
    else {
        $resolvedRemote = Resolve-RemoteTarget -HostValue $RemoteHost -UserValue $RemoteUser
        if ([string]::IsNullOrWhiteSpace($resolvedRemote.Host)) {
            Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "FAIL" -Details "-RemoteHelm requires -RemoteHost (for example, k3sv01.lab.jmal.io or jmal@k3sv01.lab.jmal.io)."
            $script:AnyFail = $true
        }
        else {
            $ssh = Get-Command ssh -ErrorAction SilentlyContinue
            if (-not $ssh) {
                Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "FAIL" -Details "SSH client is required for the explicit tier 4 check but is not installed on PATH."
                $script:AnyFail = $true
            }
            else {
                $remoteTarget = $resolvedRemote.Target
                $remoteScript = @"
set -euo pipefail
trap 'rm -f /tmp/rendered-selfservice-prod.yaml' EXIT
cd "$HOME/selfservice-api-helm"
helm lint deploy/helm/selfservice -f deploy/helm/selfservice/values.yaml
helm lint deploy/helm/selfservice -f deploy/helm/selfservice/values.yaml -f deploy/helm/selfservice/values.prod.yaml
helm template selfservice deploy/helm/selfservice -f deploy/helm/selfservice/values.yaml -f deploy/helm/selfservice/values.prod.yaml > /tmp/rendered-selfservice-prod.yaml
count=$(grep -c '^[[:space:]]*- name: SYNTHETIC_LIFECYCLE_ENABLED$' /tmp/rendered-selfservice-prod.yaml)
if [ "$count" -ne 1 ]; then
  echo "ERROR: prod render has SYNTHETIC_LIFECYCLE_ENABLED $count times, want exactly 1" >&2
  exit 1
fi
value=$(grep -A1 '^[[:space:]]*- name: SYNTHETIC_LIFECYCLE_ENABLED$' /tmp/rendered-selfservice-prod.yaml | tail -1)
if ! printf '%s\n' "$value" | grep -Eq '^[[:space:]]*value: "true"$'; then
  echo "ERROR: prod render's SYNTHETIC_LIFECYCLE_ENABLED is not explicit true: $value" >&2
  exit 1
fi
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
}

if ($script:AnyFail) {
    Write-Host "OVERALL: FAIL - at least one runnable tier failed."
    exit 1
}

Write-Host "OVERALL: PASS - all selected tiers succeeded or were intentionally skipped."
exit 0
