<#
.SYNOPSIS
    Installiert bzw. aktualisiert den Homelab-Monitor-Agenten als Windows-Dienst.

.DESCRIPTION
    Lädt mat-agent-windows-amd64.exe aus dem GitHub-Release (neueste Version oder
    -Version), prüft die SHA-256-Summe aus release.json und richtet den Dienst mit
    "mat-agent service install" ein. Liegt das Programm schon vor, geht es auch mit
    -Binary ohne Download. Das Token wird abgefragt (oder aus MAT_AGENT_TOKEN gelesen),
    damit es nicht in der Prozessliste oder der History landet.

    In einer Administrator-PowerShell ausführen. Heruntergeladene Skripte blockiert
    Windows meist; dann so starten:
        powershell -ExecutionPolicy Bypass -File .\install-agent.ps1 …

.EXAMPLE
    .\install-agent.ps1 -Server wss://10.0.0.108/ws/agent -Ca .\server.crt
    .\install-agent.ps1                        # nur Programm aktualisieren, Konfiguration bleibt
    .\install-agent.ps1 -Version v0.5.0 -Server wss://10.0.0.108/ws/agent -Ca .\server.crt -Id pc-buero
    .\install-agent.ps1 -Uninstall [-Purge]
#>
param(
    # Server-URL, z. B. wss://10.0.0.108/ws/agent. Ohne: vorhandene Konfiguration bleibt.
    [string]$Server = "",
    # Zertifikat des Servers (server.crt) für wss:// mit selbst signiertem Zertifikat.
    [string]$Ca = "",
    # Agent-ID (Standard: Hostname).
    [string]$Id = "",
    # Release-Version, z. B. v0.5.0 (Standard: neueste).
    [string]$Version = "",
    # Vorhandenes Programm statt Download.
    [string]$Binary = "",
    [switch]$Uninstall,
    # Mit -Uninstall: auch Konfiguration und Log löschen.
    [switch]$Purge,
    # Nur laden und prüfen, nichts installieren (gibt den Pfad aus).
    [switch]$DownloadOnly
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue" # Fortschrittsbalken bremst Invoke-WebRequest stark

# Quelle der Releases; MAT_RELEASE_URL nur für Tests oder Forks.
$releases = if ($env:MAT_RELEASE_URL) { $env:MAT_RELEASE_URL } else { "https://github.com/Daywalker91/Monitoring-Admin-Tool/releases" }
$name = "mat-agent-windows-amd64.exe"
$installed = Join-Path $env:ProgramFiles "mat-agent\mat-agent.exe"

function Fail($msg) { Write-Host "fehler: $msg" -ForegroundColor Red; exit 1 }
function Info($msg) { Write-Host "==> $msg" }

$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin -and -not $DownloadOnly) { Fail "bitte in einer Administrator-PowerShell ausführen" }

# --- Deinstallation ---------------------------------------------------------
if ($Uninstall) {
    if (-not (Test-Path $installed)) { Fail "$installed nicht gefunden – agent ist nicht installiert" }
    $uninstallArgs = @("service", "uninstall")
    if ($Purge) { $uninstallArgs += "-purge" }
    & $installed @uninstallArgs
    if ($LASTEXITCODE -ne 0) { Fail "deinstallation fehlgeschlagen" }
    # Das Programmverzeichnis bleibt sonst mit der laufenden Kopie zurück.
    Remove-Item -Recurse -Force (Split-Path $installed) -ErrorAction SilentlyContinue
    Info "agent entfernt"
    exit 0
}

# --- Eingaben prüfen --------------------------------------------------------
if ($Server -and $Server -notmatch '^wss?://') { Fail "-Server muss mit ws:// oder wss:// beginnen" }
if ($Version -and $Version -notmatch '^v\d{1,4}\.\d{1,4}\.\d{1,4}$') { Fail "-Version im format v1.2.3 angeben" }
if ($Binary -and $Version) { Fail "-Binary und -Version schließen sich aus" }
if ($Ca) {
    if (-not (Test-Path $Ca -PathType Leaf)) { Fail "ca-datei $Ca nicht gefunden" }
    $Ca = (Resolve-Path $Ca).Path
}
if ($Id -and $Id -notmatch '^[A-Za-z0-9._-]{1,64}$') { Fail "-Id darf nur buchstaben, ziffern, '.', '_' und '-' enthalten (max. 64)" }
if (-not $Server -and -not $DownloadOnly -and -not (Test-Path (Join-Path $env:ProgramData "mat-agent\agent.json"))) {
    Fail "noch keine konfiguration vorhanden – -Server angeben"
}
if ($env:PROCESSOR_ARCHITECTURE -notin @("AMD64", "ARM64")) { Fail "nur 64-bit-windows wird unterstützt" }

# --- Programm laden und prüfen ----------------------------------------------
$tmp = $null
$promptedToken = $false
try {
    if ($Binary) {
        if (-not (Test-Path $Binary -PathType Leaf)) { Fail "$Binary nicht gefunden" }
        $exe = (Resolve-Path $Binary).Path
    } else {
        if (-not $env:MAT_RELEASE_URL -and -not $releases.StartsWith("https://")) { Fail "releases nur über https" }
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
        $base = if ($Version) { "$releases/download/$Version" } else { "$releases/latest/download" }
        $tmp = Join-Path $env:TEMP ("mat-agent-" + [Guid]::NewGuid().ToString("N"))
        New-Item -ItemType Directory $tmp | Out-Null
        Info "lade $name ($(if ($Version) { $Version } else { 'neueste version' })) von $releases"
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/release.json" -OutFile (Join-Path $tmp "release.json")
            $exe = Join-Path $tmp $name
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile $exe
        } catch {
            Fail "laden fehlgeschlagen: $($_.Exception.Message)"
        }
        $manifest = Get-Content (Join-Path $tmp "release.json") -Raw | ConvertFrom-Json
        $entry = $manifest.files | Where-Object { $_.name -eq $name }
        if (-not $entry) { Fail "$name steht nicht in release.json" }
        $got = (Get-FileHash -Algorithm SHA256 $exe).Hash.ToLowerInvariant()
        if ($got -ne $entry.sha256) { Fail "prüfsumme von $name stimmt nicht – abbruch" }
        Info "prüfsumme ok ($($manifest.version))"
    }

    $got = (& $exe version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $got) { Fail "$exe startet nicht" }
    if ($DownloadOnly) {
        Info "geladen: $exe ($got)"
        $tmp = $null # nicht aufräumen
        exit 0
    }

    # --- Dienst einrichten ----------------------------------------------------
    $serviceArgs = @("service", "install")
    if ($Server) {
        if (-not $env:MAT_AGENT_TOKEN) {
            $secure = Read-Host "agent-token" -AsSecureString
            $ptr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
            $promptedToken = $true
            try { $env:MAT_AGENT_TOKEN = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr) }
            finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr) }
        }
        if (-not $env:MAT_AGENT_TOKEN) { Fail "token fehlt" }
        $serviceArgs += @("-server", $Server)
    }
    if ($Ca) { $serviceArgs += @("-ca", $Ca) }
    if ($Id) { $serviceArgs += @("-id", $Id) }
    Info "dienst einrichten ($got)"
    & $exe @serviceArgs
    if ($LASTEXITCODE -ne 0) { Fail "service install fehlgeschlagen" }
    & $installed service status
    Info "fertig – log: $(Join-Path $env:ProgramData 'mat-agent\agent.log')"
} finally {
    if ($promptedToken) { Remove-Item Env:MAT_AGENT_TOKEN -ErrorAction SilentlyContinue }
    if ($tmp) { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }
}
