# Fails if private-lab fingerprints appear in the public selfservice-api tree.
# Excludes .git and the generated wiki bundle (regenerate with make wiki-bundle).
#
# Usage:
#   pwsh ./scripts/check-public-fingerprints.ps1
#   pwsh ./scripts/check-public-fingerprints.ps1 -Root .

[CmdletBinding()]
param(
    [string]$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
)

$ErrorActionPreference = "Stop"

$patterns = @(
    @{ Name = "mgmt-vlan"; Regex = '10\.10\.10\.' },
    @{ Name = "home-lan"; Regex = '192\.168\.68\.' },
    @{ Name = "esxi-lab-fqdn"; Regex = 'esxi[0-9]\.lab\.jmal\.io' },
    # Split literals so this script does not match itself.
    @{ Name = "build-password"; Regex = ('Change' + 'me123!') },
    @{ Name = "admin-password"; Regex = ('Build' + 'Admin123!') },
    @{ Name = "home-jmal-path"; Regex = ('/home/' + 'jmal') }
)

# Include internal/docs/_bundle so a stale wiki mirror cannot hide fingerprints.
$excludeDirNames = @(".git", "node_modules", ".agent-scratch")
$selfName = "check-public-fingerprints.ps1"

$hits = New-Object System.Collections.Generic.List[string]
$files = Get-ChildItem -LiteralPath $Root -Recurse -File -Force | Where-Object {
    $rel = $_.FullName.Substring($Root.Length).TrimStart('\', '/')
    foreach ($d in $excludeDirNames) {
        if ($rel -match "(^|[\\/])$([regex]::Escape($d))([\\/]|$)") { return $false }
    }
    if ($_.Name -eq $selfName) { return $false }
    return $true
}

foreach ($file in $files) {
    # Skip obviously binary extensions
    if ($file.Extension -match '\.(png|jpg|jpeg|gif|webp|ico|pdf|zip|gz|tgz|exe|dll|so|dylib|bin|wasm)$') {
        continue
    }
    $text = $null
    try {
        $text = [IO.File]::ReadAllText($file.FullName)
    } catch {
        continue
    }
    foreach ($p in $patterns) {
        if ([regex]::IsMatch($text, $p.Regex)) {
            $rel = $file.FullName.Substring($Root.Length).TrimStart('\', '/')
            $hits.Add(("{0}: {1}" -f $p.Name, $rel))
        }
    }
}

if ($hits.Count -gt 0) {
    Write-Host "Public fingerprint check FAILED:" -ForegroundColor Red
    $hits | Sort-Object -Unique | ForEach-Object { Write-Host "  $_" }
    Write-Host ""
    Write-Host "Product public hosts (crucible.jmal.io, authentik.jmal.io, etc.) are allowlisted by omission."
    Write-Host "Real lab topology belongs in private crucible-deploy / the ops vault."
    exit 1
}

Write-Host "Public fingerprint check PASS"
exit 0
