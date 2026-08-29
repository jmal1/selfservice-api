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

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))
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
    if (-not (Test-Path -LiteralPath $makefilePath)) {
        throw "Makefile not found at $makefilePath"
    }

    $collect = $false
    $seeds = @()
    foreach ($line in Get-Content -LiteralPath $makefilePath) {
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

    return ,$seeds
}

function Assert-WikiSeedSetMatchesMakefile {
    param(
        [string[]]$SeedPaths,
        [string[]]$ExpectedSeeds
    )

    if ($SeedPaths.Count -ne $ExpectedSeeds.Count) {
        throw "WIKI_SEEDS drifted: Makefile defines $($SeedPaths.Count) seeds but the local verifier expects $($ExpectedSeeds.Count)."
    }

    $normalizedActual = @($SeedPaths | ForEach-Object { [string]$_ } | ForEach-Object { $_.Trim() } | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Sort-Object -Unique)
    $normalizedExpected = @($ExpectedSeeds | ForEach-Object { [string]$_ } | ForEach-Object { $_.Trim() } | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Sort-Object -Unique)

    $missing = @($normalizedExpected | Where-Object { $normalizedActual -notcontains $_ })
    if ($missing.Count -gt 0) {
        throw "WIKI_SEEDS drifted: missing expected seed(s): $($missing -join ', ')"
    }

    $extra = @($normalizedActual | Where-Object { $normalizedExpected -notcontains $_ })
    if ($extra.Count -gt 0) {
        throw "WIKI_SEEDS drifted: unexpected seed(s): $($extra -join ', ')"
    }
}

function Get-ToolCommand {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ToolName
    )

    $normalized = [string]$ToolName
    if (-not [string]::IsNullOrWhiteSpace($normalized)) {
        $normalized = $normalized.Trim()
        if ($normalized.EndsWith('.exe', [System.StringComparison]::OrdinalIgnoreCase)) {
            $normalized = $normalized.Substring(0, $normalized.Length - 4)
        }
        $normalized = $normalized.ToLowerInvariant()

        $forcedMissing = [Environment]::GetEnvironmentVariable('LOCAL_VERIFY_FORCE_MISSING_TOOLS')
        if (-not [string]::IsNullOrWhiteSpace($forcedMissing)) {
            foreach ($forced in ($forcedMissing -split ',')) {
                $candidate = ($forced.Trim())
                if ([string]::IsNullOrWhiteSpace($candidate)) {
                    continue
                }
                if ($candidate.EndsWith('.exe', [System.StringComparison]::OrdinalIgnoreCase)) {
                    $candidate = $candidate.Substring(0, $candidate.Length - 4)
                }
                if ($candidate.ToLowerInvariant() -eq $normalized) {
                    return $null
                }
            }
        }
    }

    return Get-Command -Name $ToolName -ErrorAction SilentlyContinue
}

function Format-TrimmedOutput {
    param(
        [Parameter(ValueFromPipeline = $true)]
        [AllowNull()]
        $Value
    )

    if ($null -eq $Value) {
        return ""
    }

    $text = [string]$Value
    if ([string]::IsNullOrWhiteSpace($text)) {
        return ""
    }

    return $text.Trim()
}

function Invoke-GitBytes {
    param(
        [string]$Root,
        [string[]]$Arguments
    )

    $startInfo = [System.Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = "git"
    $startInfo.WorkingDirectory = $Root
    $startInfo.UseShellExecute = $false
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    foreach ($argument in $Arguments) {
        $startInfo.ArgumentList.Add($argument)
    }

    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    $buffer = [System.IO.MemoryStream]::new()
    try {
        if (-not $process.Start()) {
            throw "Could not start git $($Arguments -join ' ')."
        }
        $errorTask = $process.StandardError.ReadToEndAsync()
        $process.StandardOutput.BaseStream.CopyTo($buffer)
        $process.WaitForExit()
        $errorText = $errorTask.GetAwaiter().GetResult()
        if ($process.ExitCode -ne 0) {
            throw "git $($Arguments -join ' ') failed: $(Format-TrimmedOutput $errorText)"
        }
        return ,$buffer.ToArray()
    }
    finally {
        $buffer.Dispose()
        $process.Dispose()
    }
}

function Get-GitPathList {
    param(
        [string]$Root,
        [string[]]$Arguments
    )

    $bytes = Invoke-GitBytes -Root $Root -Arguments $Arguments
    $paths = [System.Collections.Generic.List[string]]::new()
    $start = 0
    for ($index = 0; $index -lt $bytes.Length; $index++) {
        if ($bytes[$index] -ne 0) {
            continue
        }
        if ($index -gt $start) {
            $paths.Add([System.Text.Encoding]::UTF8.GetString($bytes, $start, $index - $start))
        }
        $start = $index + 1
    }
    if ($start -ne $bytes.Length) {
        throw "git path output was not NUL-terminated."
    }
    return ,$paths.ToArray()
}

function Get-GitIndexBlobBytes {
    param(
        [string]$Root,
        [string]$RepoPath
    )

    return ,(Invoke-GitBytes -Root $Root -Arguments @("show", "--no-textconv", ":$RepoPath"))
}

function Get-CRLFNormalizedBytes {
    param(
        [byte[]]$Bytes
    )

    $normalized = [System.IO.MemoryStream]::new()
    try {
        for ($index = 0; $index -lt $Bytes.Length; $index++) {
            if ($Bytes[$index] -eq 13 -and ($index + 1) -lt $Bytes.Length -and $Bytes[$index + 1] -eq 10) {
                continue
            }
            $normalized.WriteByte($Bytes[$index])
        }
        return ,$normalized.ToArray()
    }
    finally {
        $normalized.Dispose()
    }
}

function Test-ByteArraysEqual {
    param(
        [byte[]]$Left,
        [byte[]]$Right
    )

    if ($Left.Length -ne $Right.Length) {
        return $false
    }
    for ($index = 0; $index -lt $Left.Length; $index++) {
        if ($Left[$index] -ne $Right[$index]) {
            return $false
        }
    }
    return $true
}

function Test-GoSourceIsFormatted {
    param(
        [byte[]]$Source,
        [string]$DisplayPath
    )

    $tempPath = Join-Path ([System.IO.Path]::GetTempPath()) ("local-verify-gofmt-" + [System.Guid]::NewGuid().ToString() + ".go")
    [System.IO.File]::WriteAllBytes($tempPath, $Source)
    try {
        $gofmtOutput = @(& gofmt -w $tempPath 2>&1)
        if ($LASTEXITCODE -ne 0) {
            throw "gofmt failed for $DisplayPath`: $(Format-TrimmedOutput ($gofmtOutput | Out-String))"
        }
        $formatted = [System.IO.File]::ReadAllBytes($tempPath)
        $normalizedSource = Get-CRLFNormalizedBytes -Bytes $Source
        $normalizedFormatted = Get-CRLFNormalizedBytes -Bytes $formatted
        return Test-ByteArraysEqual -Left $normalizedSource -Right $normalizedFormatted
    }
    finally {
        Remove-Item -LiteralPath $tempPath -Force -ErrorAction SilentlyContinue
    }
}

function Get-GoFormattingProblems {
    param(
        [string]$Root
    )

    $stagedPaths = Get-GitPathList -Root $Root -Arguments @("diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR", "--", "*.go")
    $worktreePaths = @(
        (Get-GitPathList -Root $Root -Arguments @("diff", "--name-only", "-z", "--diff-filter=ACMR", "--", "*.go")) +
        (Get-GitPathList -Root $Root -Arguments @("ls-files", "--others", "--exclude-standard", "-z", "--", "*.go")) |
            Sort-Object -Unique
    )
    $problems = @()

    foreach ($path in $stagedPaths) {
        $source = Get-GitIndexBlobBytes -Root $Root -RepoPath $path
        if (-not (Test-GoSourceIsFormatted -Source $source -DisplayPath "$path (staged)")) {
            $problems += "$path (staged)"
        }
    }

    foreach ($path in $worktreePaths) {
        $fullPath = [System.IO.Path]::GetFullPath((Join-Path $Root $path))
        if (-not [System.IO.File]::Exists($fullPath)) {
            continue
        }
        $source = [System.IO.File]::ReadAllBytes($fullPath)
        if (-not (Test-GoSourceIsFormatted -Source $source -DisplayPath $path)) {
            $problems += $path
        }
    }

    return ,$problems
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
    foreach ($file in Get-ChildItem -LiteralPath $Root -Recurse -File) {
        $rel = [System.IO.Path]::GetRelativePath($Root, $file.FullName)
        $hash = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash
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

    return ,$mismatches
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
    # Tier 0 uses Git from the current checkout and requires only Go plus gofmt as additional tools.
    $goCmd = Get-ToolCommand -ToolName "go"
    $gofmtCmd = Get-ToolCommand -ToolName "gofmt"
    if (-not $goCmd -or -not $gofmtCmd) {
        $missing = @()
        if (-not $goCmd) { $missing += "Go toolchain" }
        if (-not $gofmtCmd) { $missing += "gofmt" }
        if ($explicit) {
            Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (($missing -join " and ") + " is required for the explicit tier 0 check but is not installed on PATH.")
            $script:AnyFail = $true
        }
        else {
            Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "SKIP" -Details (($missing -join " and ") + " not installed; auto-detected tier 0 is skipped.")
        }
    }
    else {
        Push-Location -LiteralPath $repoRoot
        try {
            $gofmtProblems = Get-GoFormattingProblems -Root $repoRoot
            if ($gofmtProblems.Count -gt 0) {
                Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details ("gofmt failed: " + (Format-TrimmedOutput ($gofmtProblems | Out-String)))
                $script:AnyFail = $true
            }
            else {
                $buildOutput = & go build ./... 2>&1
                if ($LASTEXITCODE -ne 0) {
                    Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (Format-TrimmedOutput ($buildOutput | Out-String))
                    $script:AnyFail = $true
                }
                else {
                    $vetOutput = & go vet ./... 2>&1
                    if ($LASTEXITCODE -ne 0) {
                        Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (Format-TrimmedOutput ($vetOutput | Out-String))
                        $script:AnyFail = $true
                    }
                    else {
                        $testOutput = & go test ./... -short -count=1 2>&1
                        if ($LASTEXITCODE -ne 0) {
                            Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (Format-TrimmedOutput ($testOutput | Out-String))
                            $script:AnyFail = $true
                        }
                        else {
                            Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "PASS" -Details "gofmt, build, vet, and short Go tests succeeded."
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
    $goCmd = Get-ToolCommand -ToolName "go"
    $wslCmd = Get-ToolCommand -ToolName "wsl.exe"

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
        [System.IO.Directory]::CreateDirectory($tmpDir) | Out-Null
        $binaryPath = Join-Path $tmpDir "provisioning-linux.test"
        $pkgPath = Join-Path $repoRoot "internal/provisioning"
        $savedGOOS = $env:GOOS
        $savedGOARCH = $env:GOARCH
        $savedCGO = $env:CGO_ENABLED
        try {
            $env:GOOS = "linux"
            $env:GOARCH = "amd64"
            $env:CGO_ENABLED = "0"

            $compileOutput = & go test -c -o $binaryPath ./internal/provisioning 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (Format-TrimmedOutput ($compileOutput | Out-String))
                $script:AnyFail = $true
            }
            else {
                $wslPkgPath = & wsl.exe -e wslpath -u -- $pkgPath 2>&1
                if ($LASTEXITCODE -ne 0) {
                    Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (Format-TrimmedOutput ($wslPkgPath | Out-String))
                    $script:AnyFail = $true
                }
                else {
                    $wslBinaryPath = & wsl.exe -e wslpath -u -- $binaryPath 2>&1
                    if ($LASTEXITCODE -ne 0) {
                        Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (Format-TrimmedOutput ($wslBinaryPath | Out-String))
                        $script:AnyFail = $true
                    }
                    else {
                        $wslPkgPath = Format-TrimmedOutput ($wslPkgPath | Out-String)
                        $wslBinaryPath = Format-TrimmedOutput ($wslBinaryPath | Out-String)
                        $runOutput = & wsl.exe -e bash -lc 'cd "$1" && chmod +x "$2" && "$2" -test.v' _ $wslPkgPath $wslBinaryPath 2>&1
                        if ($LASTEXITCODE -ne 0) {
                            Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (Format-TrimmedOutput ($runOutput | Out-String))
                            $script:AnyFail = $true
                        }
                        else {
                            Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "PASS" -Details "Linux cross-compile and POSIX deploy-script tests passed under WSL."
                        }
                    }
                }
            }
        }
        catch {
            Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details $_.Exception.Message
            $script:AnyFail = $true
        }
        finally {
            if (Test-Path -LiteralPath $binaryPath) {
                Remove-Item -LiteralPath $binaryPath -Force
            }
            if ($null -ne $savedGOOS) { $env:GOOS = $savedGOOS } else { Remove-Item Env:GOOS -ErrorAction SilentlyContinue }
            if ($null -ne $savedGOARCH) { $env:GOARCH = $savedGOARCH } else { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue }
            if ($null -ne $savedCGO) { $env:CGO_ENABLED = $savedCGO } else { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue }
        }
    }
}

if ($selectedTiers -contains 2) {
    $explicit = ($Tier -ne "all")
    $goCmd = Get-ToolCommand -ToolName "go"
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
        [System.IO.Directory]::CreateDirectory($tempBundleDir) | Out-Null

        $bundleArgs = @("run","./cmd/wiki-bundler","-repo-root",".","-out",$tempBundleDir)
        foreach ($seed in $seedPaths) {
            $bundleArgs += @("-seed", $seed)
        }

        Push-Location -LiteralPath $repoRoot
        try {
            $bundleOutput = & go @bundleArgs 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-TierResult -Index 2 -Name "wiki bundle + byte-verify" -State "FAIL" -Details (Format-TrimmedOutput ($bundleOutput | Out-String))
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
            Remove-Item -LiteralPath $tempBundleDir -Recurse -Force -ErrorAction SilentlyContinue
            Pop-Location
        }
    }
}

if ($selectedTiers -contains 3) {
    $explicit = ($Tier -ne "all")
    $docker = Get-ToolCommand -ToolName "docker"
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
        Push-Location -LiteralPath $repoRoot
        try {
            $composeOutput = & docker compose -f docker-compose.dev.yaml up -d --wait 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details (Format-TrimmedOutput ($composeOutput | Out-String))
                $script:AnyFail = $true
            }
            else {
                $hasIntegrationBuild = $false
                foreach ($file in Get-ChildItem -LiteralPath $repoRoot -Recurse -Filter *.go) {
                    $content = Get-Content -LiteralPath $file.FullName -Raw -ErrorAction SilentlyContinue
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
                        Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details (Format-TrimmedOutput ($testOutput | Out-String))
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
                Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details ("Docker cleanup failed: " + (Format-TrimmedOutput ($downOutput | Out-String)))
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
            $ssh = Get-ToolCommand -ToolName "ssh"
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
                    Write-TierResult -Index 4 -Name "production Helm render + lint via Vault SSH" -State "FAIL" -Details (Format-TrimmedOutput ($remoteOutput | Out-String))
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
