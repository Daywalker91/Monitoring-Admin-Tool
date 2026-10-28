<#
.SYNOPSIS
    Baut den Agenten (und optional den Server) für alle Zielplattformen nach .\dist.

.EXAMPLE
    .\build.ps1
    .\build.ps1 -Version v0.5.0 -Server

.NOTES
    Releases baut und signiert .github/workflows/release.yml (bei einem Tag v*).
#>
param(
    # Versionskennung, die ins Binary geschrieben wird (Standard: git describe).
    [string]$Version = "",
    # Zusätzlich den Server für linux/amd64, linux/arm64 und windows/amd64 bauen.
    [switch]$Server
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $goDefault = "C:\Program Files\Go\bin"
    if (Test-Path "$goDefault\go.exe") { $env:Path = "$goDefault;$env:Path" }
    else { throw "go wurde nicht gefunden." }
}

if (-not $Version) {
    $Version = (git describe --tags --always --dirty 2>$null)
    if (-not $Version) { $Version = "dev" }
}

$targets = @(
    @{ OS = "linux";   Arch = "amd64"; Arm = "" },
    @{ OS = "linux";   Arch = "arm64"; Arm = "" },
    @{ OS = "linux";   Arch = "arm";   Arm = "7" },
    @{ OS = "windows"; Arch = "amd64"; Arm = "" }
)

$dist = Join-Path $PSScriptRoot "dist"
New-Item -ItemType Directory -Force $dist | Out-Null

# Umgebungsvariablen sichern, damit die aufrufende Shell unverändert bleibt.
$saved = @{}
foreach ($name in "GOOS", "GOARCH", "GOARM", "CGO_ENABLED") { $saved[$name] = [Environment]::GetEnvironmentVariable($name, "Process") }

function Build-Binary($pkg, $name, $t) {
    $suffix = if ($t.Arm) { "v$($t.Arm)" } else { "" }
    $ext = if ($t.OS -eq "windows") { ".exe" } else { "" }
    $out = Join-Path $dist "$name-$($t.OS)-$($t.Arch)$suffix$ext"

    $env:GOOS = $t.OS
    $env:GOARCH = $t.Arch
    $env:GOARM = $t.Arm
    $env:CGO_ENABLED = "0"

    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $out $pkg
    if ($LASTEXITCODE -ne 0) { throw "Build fehlgeschlagen: $out" }
    $size = [math]::Round((Get-Item $out).Length / 1MB, 1)
    Write-Host ("  {0,-40} {1,6} MB" -f (Split-Path $out -Leaf), $size)
}

try {
    Write-Host "Agent $Version"
    foreach ($t in $targets) { Build-Binary "./agent/cmd/mat-agent" "mat-agent" $t }

    if ($Server) {
        Write-Host "Server $Version"
        foreach ($t in $targets | Where-Object { $_.Arch -ne "arm" }) { Build-Binary "./server/cmd/mat-server" "mat-server" $t }
    }
}
finally {
    foreach ($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name, $saved[$name], "Process") }
}

# Installationsdateien für Linux dazulegen: dist\ auf das Zielsystem kopieren genügt.
# Pfade mit "/", damit das Skript auch mit PowerShell 7 unter Linux läuft (Release-Workflow).
Copy-Item (Join-Path $PSScriptRoot "deploy/install-agent.sh") $dist
Copy-Item (Join-Path $PSScriptRoot "deploy/systemd/mat-agent.service") $dist

Write-Host "Fertig: $dist"
