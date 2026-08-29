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

function Quote-BashSingleQuoted {
    param(
        [AllowEmptyString()]
        [string]$Value
    )

    if ($null -eq $Value) {
        return "''"
    }

    $text = [string]$Value
    $replacement = [string]::Concat("'", '"', "'", '"', "'")
    return "'" + $text.Replace("'", $replacement) + "'"
}

function Normalize-WslPath {
    param(
        [AllowEmptyString()]
        [string]$Value
    )

    if ($null -eq $Value) {
        return ""
    }

    $text = [string]$Value
    $text = $text.Trim()
    if ([string]::IsNullOrWhiteSpace($text)) {
        return ""
    }

    return $text.Replace('\\', '/').TrimEnd('/')
}

function Join-WslPath {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$Segments
    )

    $parts = [System.Collections.Generic.List[string]]::new()
    foreach ($segment in $Segments) {
        if ($null -eq $segment) {
            continue
        }

        $text = [string]$segment
        if ([string]::IsNullOrWhiteSpace($text)) {
            continue
        }

        $text = $text.Trim().TrimEnd('/').TrimEnd('\\')
        if (-not [string]::IsNullOrWhiteSpace($text)) {
            $parts.Add($text)
        }
    }

    if ($parts.Count -eq 0) {
        return ""
    }

    return (Normalize-WslPath -Value ([string]::Join('/', $parts)))
}

function Convert-WindowsPathToWsl {
    param(
        [Parameter(Mandatory = $true)]
        [string]$WindowsPath
    )

    $result = & wsl.exe @('-e', 'wslpath', '-u', '--', $WindowsPath) 2>&1 | Out-String
    if ((Get-LastExitCodeValue) -ne 0) {
        throw "wsl.exe could not translate the Windows repo path '$WindowsPath'."
    }

    return Normalize-WslPath -Value (Format-TrimmedOutput $result)
}

function Get-GoFormattingTargets {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Root
    )

    $gitCmd = Get-Command -Name git -ErrorAction SilentlyContinue
    if (-not $gitCmd) {
        return @()
    }

    $candidates = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    Push-Location $Root
    try {
        foreach ($args in @(
            @('diff', '--name-only', '--diff-filter=ACMR', '--', '*.go'),
            @('diff', '--cached', '--name-only', '--diff-filter=ACMR', '--', '*.go'),
            @('ls-files', '--others', '--exclude-standard', '--', '*.go')
        )) {
            $output = & git @args 2>$null
            if ((Get-LastExitCodeValue) -ne 0) {
                continue
            }

            foreach ($line in @($output)) {
                $path = [string]$line
                if ([string]::IsNullOrWhiteSpace($path)) {
                    continue
                }
                $path = $path.Trim()
                if ($path.EndsWith('.go', [System.StringComparison]::OrdinalIgnoreCase)) {
                    $null = $candidates.Add($path)
                }
            }
        }
    }
    finally {
        Pop-Location
    }

    $result = [System.Collections.Generic.List[string]]::new()
    foreach ($path in ($candidates | Sort-Object)) {
        $result.Add($path)
    }
    Write-Output -NoEnumerate @($result)
}

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
    $seedList = [System.Collections.Generic.List[string]]::new()
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
                $seedList.Add($seed)
            }
        }
    }

    if ($seedList.Count -eq 0) {
        throw "Could not parse WIKI_SEEDS from $makefilePath"
    }

    Write-Output -NoEnumerate @($seedList)
}

function Assert-WikiSeedSetMatchesMakefile {
    param(
        [string[]]$SeedPaths,
        [string[]]$ExpectedSeeds
    )

    if ($SeedPaths.Count -ne $ExpectedSeeds.Count) {
        throw "WIKI_SEEDS drifted: Makefile defines $($SeedPaths.Count) seeds but the local verifier expects $($ExpectedSeeds.Count)."
    }

    $normalizedActual = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($seed in $SeedPaths) {
        $value = [string]$seed
        $value = $value.Trim()
        if (-not [string]::IsNullOrWhiteSpace($value)) {
            $null = $normalizedActual.Add($value)
        }
    }

    $normalizedExpected = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($seed in $ExpectedSeeds) {
        $value = [string]$seed
        $value = $value.Trim()
        if (-not [string]::IsNullOrWhiteSpace($value)) {
            $null = $normalizedExpected.Add($value)
        }
    }

    $missing = [System.Collections.Generic.List[string]]::new()
    foreach ($expected in $normalizedExpected) {
        if (-not $normalizedActual.Contains($expected)) {
            $missing.Add($expected)
        }
    }
    if ($missing.Count -gt 0) {
        throw "WIKI_SEEDS drifted: missing expected seed(s): $($missing -join ', ')"
    }

    $extra = [System.Collections.Generic.List[string]]::new()
    foreach ($actual in $normalizedActual) {
        if (-not $normalizedExpected.Contains($actual)) {
            $extra.Add($actual)
        }
    }
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

function Get-LastExitCodeValue {
    $value = Get-Variable -Name LASTEXITCODE -ValueOnly -ErrorAction SilentlyContinue
    if ($null -eq $value) {
        return 0
    }
    return [int]$value
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

function Normalize-LfText {
    param(
        [AllowEmptyString()]
        [string]$Value
    )

    if ($null -eq $Value) {
        return ""
    }

    $text = [string]$Value
    return $text.Replace("`r`n", "`n").Replace("`r", "`n")
}

function Test-GoFormattedText {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Content,
        [Parameter(Mandatory = $true)]
        [string]$Label
    )

    $normalized = Normalize-LfText -Value $Content
    $tempPath = Join-Path ([System.IO.Path]::GetTempPath()) ("gofmt-check-" + [System.Guid]::NewGuid().ToString("N") + ".go")
    try {
        [System.IO.File]::WriteAllText($tempPath, $normalized, [System.Text.UTF8Encoding]::new($false))
        $beforeBytes = [System.IO.File]::ReadAllBytes($tempPath)
        $gofmtOutput = & gofmt -w $tempPath 2>&1
        if ((Get-LastExitCodeValue) -ne 0) {
            return "gofmt exited $(Get-LastExitCodeValue) for ${Label}: $(Format-TrimmedOutput ($gofmtOutput | Out-String))"
        }

        $afterBytes = [System.IO.File]::ReadAllBytes($tempPath)
        if ($beforeBytes.Length -ne $afterBytes.Length) {
            return "gofmt would rewrite $Label"
        }

        for ($i = 0; $i -lt $beforeBytes.Length; $i++) {
            if ($beforeBytes[$i] -ne $afterBytes[$i]) {
                return "gofmt would rewrite $Label"
            }
        }

        return $null
    }
    finally {
        Remove-Item -Path $tempPath -Force -ErrorAction SilentlyContinue
    }
}

function Get-GitIndexBlobText {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Root,
        [Parameter(Mandatory = $true)]
        [string]$GitPath
    )

    $tempPath = Join-Path ([System.IO.Path]::GetTempPath()) ("git-index-" + [System.Guid]::NewGuid().ToString("N") + ".blob")
    try {
        & git -C $Root show (":" + $GitPath) 2>$null > $tempPath
        if ((Get-LastExitCodeValue) -ne 0) {
            return $null
        }

        if (-not (Test-Path -Path $tempPath -PathType Leaf)) {
            return $null
        }

        $bytes = [System.IO.File]::ReadAllBytes($tempPath)
        if ($bytes.Length -eq 0) {
            return ""
        }

        return [System.Text.Encoding]::UTF8.GetString($bytes)
    }
    finally {
        Remove-Item -Path $tempPath -Force -ErrorAction SilentlyContinue
    }
}

function Get-GoFormattingProblems {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Root,
        [string[]]$Targets = @()
    )

    $problems = [System.Collections.Generic.List[string]]::new()
    foreach ($target in $Targets) {
        $actualPath = if ([System.IO.Path]::IsPathRooted($target)) { [System.IO.Path]::GetFullPath($target) } else { Join-Path $Root $target }
        $relativePath = if ([System.IO.Path]::IsPathRooted($target)) { [System.IO.Path]::GetRelativePath($Root, $actualPath) } else { $target }
        $gitPath = $relativePath.Replace('\\', '/').Replace('\', '/')

        if (Test-Path -Path $actualPath -PathType Leaf) {
            $workingContent = Get-Content -Path $actualPath -Raw -ErrorAction SilentlyContinue
            if ($null -ne $workingContent) {
                $issue = Test-GoFormattedText -Content ([string]$workingContent) -Label "$gitPath (working tree)"
                if ($null -ne $issue) {
                    $problems.Add($issue)
                }
            }
        }

        $stagedContent = Get-GitIndexBlobText -Root $Root -GitPath $gitPath
        if ($null -ne $stagedContent) {
            $issue = Test-GoFormattedText -Content $stagedContent -Label "$gitPath (staged/index)"
            if ($null -ne $issue) {
                $problems.Add($issue)
            }
        }
    }

    Write-Output -NoEnumerate @($problems)
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
    $allKeys = @(@($actualMap.Keys) + @($expectedMap.Keys) | Select-Object -Unique)
    $mismatchList = [System.Collections.Generic.List[string]]::new()

    foreach ($key in $allKeys) {
        if (-not $actualMap.ContainsKey($key)) {
            $mismatchList.Add("$key (missing in generated bundle)")
            continue
        }
        if (-not $expectedMap.ContainsKey($key)) {
            $mismatchList.Add("$key (missing in committed bundle)")
            continue
        }
        if ($actualMap[$key] -ne $expectedMap[$key]) {
            $mismatchList.Add("$key (sha256 mismatch)")
        }
    }

    Write-Output -NoEnumerate @($mismatchList)
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

    Write-Output -NoEnumerate @($wanted | Sort-Object -Unique)
}

if ($MyInvocation.InvocationName -eq ".") {
    return
}

$selectedTiers = Get-SelectedTiers -SelectedTier $Tier -RemoteCheckRequested ([bool]$RemoteHelm)
if ($Tier -eq "4") {
    $selectedTiers = @(4)
}

if ($selectedTiers -contains 0) {
    $explicit = ($Tier -ne "all")
    $goCmd = Get-ToolCommand -ToolName "go"
    $gofmtCmd = Get-ToolCommand -ToolName "gofmt"
    if (-not $goCmd -or -not $gofmtCmd) {
        $missing = @()
        if (-not $goCmd) { $missing += "Go toolchain" }
        if (-not $gofmtCmd) { $missing += "gofmt" }
        $detail = ($missing -join " and ") + " is required for the explicit tier 0 check but is not installed on PATH."
        if ($explicit) {
            Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "FAIL" -Details $detail
            $script:AnyFail = $true
        }
        else {
            Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "SKIP" -Details ($missing -join ", ") + " not detected; auto-detected tier 0 is skipped."
        }
    }
    else {
        Push-Location $repoRoot
        try {
            $gofmtTargets = [System.Collections.Generic.List[string]]::new()
            foreach ($candidate in (Get-GoFormattingTargets -Root $repoRoot)) {
                if ($candidate -and -not [string]::IsNullOrWhiteSpace([string]$candidate)) {
                    $gofmtTargets.Add($candidate)
                }
            }

            if ($gofmtTargets.Count -gt 0) {
                $gofmtProblems = Get-GoFormattingProblems -Root $repoRoot -Targets @($gofmtTargets)
                if ($gofmtProblems.Count -gt 0) {
                    $detail = Format-TrimmedOutput ($gofmtProblems | Out-String)
                    Write-TierResult -Index 0 -Name "gofmt + go vet + short Go tests" -State "FAIL" -Details "gofmt failed: $detail"
                    $script:AnyFail = $true
                }
                else {
                    $buildOutput = & go build ./... 2>&1
                    if ((Get-LastExitCodeValue) -ne 0) {
                        Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (Format-TrimmedOutput ($buildOutput | Out-String))
                        $script:AnyFail = $true
                    }
                    else {
                        $vetOutput = & go vet ./... 2>&1
                        if ((Get-LastExitCodeValue) -ne 0) {
                            Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (Format-TrimmedOutput ($vetOutput | Out-String))
                            $script:AnyFail = $true
                        }
                        else {
                            $testOutput = & go test ./... -short -count=1 2>&1
                            if ((Get-LastExitCodeValue) -ne 0) {
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
            else {
                $buildOutput = & go build ./... 2>&1
                if ((Get-LastExitCodeValue) -ne 0) {
                    Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (Format-TrimmedOutput ($buildOutput | Out-String))
                    $script:AnyFail = $true
                }
                else {
                    $vetOutput = & go vet ./... 2>&1
                    if ((Get-LastExitCodeValue) -ne 0) {
                        Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (Format-TrimmedOutput ($vetOutput | Out-String))
                        $script:AnyFail = $true
                    }
                    else {
                        $testOutput = & go test ./... -short -count=1 2>&1
                        if ((Get-LastExitCodeValue) -ne 0) {
                            Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "FAIL" -Details (Format-TrimmedOutput ($testOutput | Out-String))
                            $script:AnyFail = $true
                        }
                        else {
                            Write-TierResult -Index 0 -Name "gofmt + go build + go vet + short Go tests" -State "PASS" -Details "No changed/untracked Go files required formatting; build, vet, and short Go tests succeeded."
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
            if ((Get-LastExitCodeValue) -ne 0) {
                Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (Format-TrimmedOutput ($compileOutput | Out-String))
                $script:AnyFail = $true
            }
            else {
                $wslRepo = Convert-WindowsPathToWsl -WindowsPath $repoRoot
                $wslPkgDir = Join-WslPath -Segments @($wslRepo, 'internal', 'provisioning')
                $wslBinary = Join-WslPath -Segments @($wslRepo, '.tmp', 'provisioning-linux.test')
                $bashScript = 'cd "$1" && chmod +x "$2" && "$2" -test.v'
                $runOutput = & wsl.exe @('-e', 'bash', '-lc', $bashScript, '_', $wslPkgDir, $wslBinary) 2>&1 | Out-String
                if ((Get-LastExitCodeValue) -ne 0) {
                    Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "FAIL" -Details (Format-TrimmedOutput $runOutput)
                    $script:AnyFail = $true
                }
                else {
                    Write-TierResult -Index 1 -Name "internal/provisioning Linux deploy-script tests" -State "PASS" -Details "Linux cross-compile and POSIX deploy-script tests passed under WSL."
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
        New-Item -ItemType Directory -Path $tempBundleDir -Force | Out-Null

        $bundleArgs = @("run","./cmd/wiki-bundler","-repo-root",".","-out",$tempBundleDir)
        foreach ($seed in $seedPaths) {
            $bundleArgs += @("-seed", $seed)
        }

        Push-Location $repoRoot
        try {
            $bundleOutput = & go @bundleArgs 2>&1
            if ((Get-LastExitCodeValue) -ne 0) {
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
            Remove-Item -Path $tempBundleDir -Recurse -Force -ErrorAction SilentlyContinue
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
        Push-Location $repoRoot
        try {
            $composeOutput = & docker compose -f docker-compose.dev.yaml up -d --wait 2>&1
            if ((Get-LastExitCodeValue) -ne 0) {
                Write-TierResult -Index 3 -Name "docker-compose.dev plus integration tests" -State "FAIL" -Details (Format-TrimmedOutput ($composeOutput | Out-String))
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
                    if ((Get-LastExitCodeValue) -ne 0) {
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
            if ((Get-LastExitCodeValue) -ne 0) {
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
                if ((Get-LastExitCodeValue) -ne 0) {
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
