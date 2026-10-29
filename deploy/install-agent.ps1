<#
.SYNOPSIS
    Installiert bzw. aktualisiert den Agenten des Monitoring and Administration Tool (MAT) als Windows-Dienst.

.DESCRIPTION
    Lädt mat-agent-windows-amd64.exe aus dem GitHub-Release (neueste Version oder
    -Version), prüft die SHA-256-Summe aus release.json und richtet den Dienst mit
    "mat-agent service install" ein. Liegt das Programm schon vor, geht es auch mit
    -Binary ohne Download. Das Token wird abgefragt (oder aus MAT_AGENT_TOKEN gelesen),
    damit es nicht in der Prozessliste oder der History landet.

    Server-Zertifikat: mit -Fingerprint (SHA-256, steht in der GUI unter Einstellungen →
    Agent installieren) holt das Skript das Zertifikat selbst vom Server und übernimmt es
    nur, wenn der Fingerabdruck stimmt. Alternativ -Ca mit der Datei server.crt. Ohne
    beides fragt das Skript bei einem nicht vertrauenswürdigen Zertifikat nach.

    In einer Administrator-PowerShell ausführen. Heruntergeladene Skripte blockiert
    Windows meist; dann so starten:
        powershell -ExecutionPolicy Bypass -File .\install-agent.ps1 …

.EXAMPLE
    .\install-agent.ps1 -Server wss://192.168.100.50/ws/agent -Fingerprint <sha256 aus der GUI>
    .\install-agent.ps1 -Server wss://192.168.100.50/ws/agent -Ca .\server.crt
    .\install-agent.ps1                        # nur Programm aktualisieren, Konfiguration bleibt
    .\install-agent.ps1 -Version v1.0.0 -Server wss://192.168.100.50/ws/agent -Ca .\server.crt -Id pc-1
    .\install-agent.ps1 -Uninstall [-Purge]
#>
param(
    # Server-URL, z. B. wss://192.168.100.50/ws/agent. Ohne: vorhandene Konfiguration bleibt.
    [string]$Server = "",
    # Zertifikat des Servers (server.crt) für wss:// mit selbst signiertem Zertifikat.
    [string]$Ca = "",
    # SHA-256-Fingerabdruck des Server-Zertifikats (hex, mit oder ohne Doppelpunkte) statt -Ca.
    [string]$Fingerprint = "",
    # Agent-ID (Standard: Hostname).
    [string]$Id = "",
    # Release-Version, z. B. v1.0.0 (Standard: neueste).
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

function Get-Sha256Hex([byte[]]$data) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash($data)) -replace "-", "").ToLowerInvariant() }
    finally { $sha.Dispose() }
}

# Get-ServerCertificate liefert das Zertifikat, das der Server beim TLS-Handshake
# vorlegt, und ob das System ihm vertraut. Der Handshake wird nur abgeschlossen,
# wenn der erwartete Fingerabdruck stimmt (bzw. ohne Vorgabe das System vertraut);
# gesendet wird ohnehin nichts.
function Get-ServerCertificate([Uri]$uri, [string]$expected) {
    $port = if ($uri.Port -gt 0) { $uri.Port } else { 443 }
    $state = @{ Raw = $null; Trusted = $false }
    $callback = {
        param($sender, $cert, $chain, $errors)
        if (-not $cert) { return $false }
        $state.Raw = $cert.GetRawCertData()
        $state.Trusted = $errors -eq [Net.Security.SslPolicyErrors]::None
        if ($expected) {
            $sha = [Security.Cryptography.SHA256]::Create()
            $hex = ([BitConverter]::ToString($sha.ComputeHash($state.Raw)) -replace "-", "").ToLowerInvariant()
            $sha.Dispose()
            return $hex -eq $expected
        }
        return $state.Trusted
    }.GetNewClosure()
    $tcp = New-Object Net.Sockets.TcpClient
    try {
        try {
            if (-not $tcp.ConnectAsync($uri.DnsSafeHost, $port).Wait(10000)) { throw "zeitüberschreitung" }
        } catch {
            Fail "keine verbindung zu $($uri.DnsSafeHost):$port – $($_.Exception.GetBaseException().Message)"
        }
        $ssl = New-Object Net.Security.SslStream -ArgumentList $tcp.GetStream(), $false, ([Net.Security.RemoteCertificateValidationCallback]$callback)
        try {
            $ssl.AuthenticateAsClient($uri.DnsSafeHost, $null, [Security.Authentication.SslProtocols]::Tls12, $false)
        } catch {
            # erwartet, wenn das Zertifikat nicht passt; ausgewertet wird unten
        } finally { $ssl.Dispose() }
    } finally { $tcp.Dispose() }
    return $state
}

# ConvertTo-Pem kodiert ein Zertifikat (DER) als PEM.
function ConvertTo-Pem([byte[]]$der) {
    $b64 = [Convert]::ToBase64String($der)
    $lines = for ($i = 0; $i -lt $b64.Length; $i += 64) { $b64.Substring($i, [Math]::Min(64, $b64.Length - $i)) }
    return "-----BEGIN CERTIFICATE-----`n" + ($lines -join "`n") + "`n-----END CERTIFICATE-----`n"
}

# Lesbarer Fingerabdruck: AA:BB:… in zwei Zeilen.
function Format-Fingerprint([string]$hex) {
    $pairs = [regex]::Matches($hex.ToUpperInvariant(), "..") | ForEach-Object { $_.Value }
    return ($pairs[0..15] -join ":") + "`n         " + ($pairs[16..31] -join ":")
}

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
if ($Fingerprint) {
    $Fingerprint = ($Fingerprint -replace "[:\s]", "").ToLowerInvariant()
    if ($Fingerprint -notmatch '^[0-9a-f]{64}$') { Fail "-Fingerprint muss ein sha-256-wert sein (64 hex-zeichen)" }
    if ($Ca) { Fail "-Fingerprint und -Ca schließen sich aus" }
    if ($Server -notmatch '^wss://') { Fail "-Fingerprint nur zusammen mit -Server wss://…" }
}
if ($Id -and $Id -notmatch '^[A-Za-z0-9._-]{1,64}$') { Fail "-Id darf nur buchstaben, ziffern, '.', '_' und '-' enthalten (max. 64)" }
if (-not $Server -and -not $DownloadOnly -and -not (Test-Path (Join-Path $env:ProgramData "mat-agent\agent.json"))) {
    Fail "noch keine konfiguration vorhanden – -Server angeben"
}
if ($env:PROCESSOR_ARCHITECTURE -notin @("AMD64", "ARM64")) { Fail "nur 64-bit-windows wird unterstützt" }

# --- Programm laden und prüfen ----------------------------------------------
$tmp = $null
$caTmp = $null
$promptedToken = $false
try {
    # --- Server-Zertifikat übernehmen -----------------------------------------
    if ($Server -match '^wss://' -and -not $Ca -and -not $DownloadOnly) {
        $cert = Get-ServerCertificate ([Uri]$Server) $Fingerprint
        if (-not $cert.Raw) { Fail "kein zertifikat vom server erhalten ($Server)" }
        $fp = Get-Sha256Hex $cert.Raw
        if ($Fingerprint) {
            if ($fp -ne $Fingerprint) { Fail "fingerabdruck des servers stimmt nicht – falscher server oder abgefangen? abbruch" }
            Info "fingerabdruck des server-zertifikats stimmt"
        } elseif ($cert.Trusted) {
            Info "server-zertifikat ist vertrauenswürdig – kein pinning nötig"
        } else {
            Write-Host "Das Zertifikat des Servers ist nicht vertrauenswürdig (selbst signiert?)."
            Write-Host "SHA-256: $(Format-Fingerprint $fp)"
            Write-Host "Vergleichen mit der GUI: Einstellungen → Agent installieren."
            if ((Read-Host "Stimmt der Fingerabdruck? (j/n)") -notmatch '^(j|ja|y|yes)$') { Fail "abgebrochen" }
        }
        if ($Fingerprint -or -not $cert.Trusted) {
            # service install kopiert die Datei nach ProgramData; die Kopie hier ist nur vorübergehend.
            $caTmp = Join-Path $env:TEMP ("mat-server-" + [Guid]::NewGuid().ToString("N") + ".crt")
            [IO.File]::WriteAllText($caTmp, (ConvertTo-Pem $cert.Raw), [Text.Encoding]::ASCII)
            $Ca = $caTmp
        }
    }

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
    if ($caTmp) { Remove-Item -Force $caTmp -ErrorAction SilentlyContinue }
    if ($tmp) { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }
}
