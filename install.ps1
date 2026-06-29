<#
.SYNOPSIS
  avairy installer for Windows — detects your arch, downloads the matching release archive,
  verifies its checksum, and installs the single `avairy.exe` (core, node, tui, and auth are
  subcommands). The PowerShell counterpart of install.sh.

.EXAMPLE
  irm https://raw.githubusercontent.com/noctarius/avairy/main/install.ps1 | iex

.EXAMPLE
  # pick a version (the | iex form can't take args, so create a scriptblock):
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/noctarius/avairy/main/install.ps1))) -Version v1.0.0

.PARAMETER Version
  Release tag to install, e.g. v1.0.0 or v1.0.0-rc1 (default: latest stable).

.PARAMETER InstallDir
  Where to put avairy.exe (default: %LOCALAPPDATA%\Programs\avairy).

.NOTES
  Environment overrides (the parameters win if both are given):
    AVAIRY_VERSION       same as -Version
    AVAIRY_INSTALL_DIR   same as -InstallDir
    AVAIRY_REPO          owner/repo to install from (default: noctarius/avairy)
#>
[CmdletBinding()]
param(
    [string]$Version = "",
    [string]$InstallDir = ""
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"   # Invoke-WebRequest is far faster without the progress bar
# PowerShell 5.1 defaults to an older protocol; GitHub requires TLS 1.2+.
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch {}

function Die($msg) { Write-Error "avairy install: $msg"; exit 1 }

$Repo = if ($env:AVAIRY_REPO) { $env:AVAIRY_REPO } else { "noctarius/avairy" }

# --- detect arch -----------------------------------------------------------
$archEnum = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
switch ($archEnum) {
    "X64"   { $Arch = "amd64" }
    "Arm64" { $Arch = "arm64" }
    default { Die "unsupported architecture '$archEnum' (avairy ships windows/amd64 and windows/arm64)" }
}

# --- resolve version -------------------------------------------------------
# Precedence: -Version > $env:AVAIRY_VERSION > latest stable release.
if (-not $Version) { $Version = $env:AVAIRY_VERSION }
if (-not $Version) {
    try {
        $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
            -Headers @{ "User-Agent" = "avairy-install" }
        $Version = $rel.tag_name
    } catch {
        Die "could not determine the latest release — pass -Version explicitly ($_)"
    }
}
if (-not $Version) { Die "could not determine a version to install" }

$Asset = "avairy_${Version}_windows_${Arch}.zip"
$Base  = "https://github.com/$Repo/releases/download/$Version"

# --- choose an install dir -------------------------------------------------
if (-not $InstallDir) { $InstallDir = $env:AVAIRY_INSTALL_DIR }
if (-not $InstallDir) { $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\avairy" }
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

# --- download + verify + install -------------------------------------------
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("avairy-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
try {
    Write-Host "avairy $Version -> windows/$Arch"
    Write-Host "downloading $Asset ..."
    $zip = Join-Path $tmp $Asset
    try { Invoke-WebRequest -Uri "$Base/$Asset" -OutFile $zip } catch { Die "download failed: $Base/$Asset" }

    try {
        $sums = (Invoke-WebRequest -Uri "$Base/SHA256SUMS" -UseBasicParsing).Content
    } catch { $sums = $null }

    if ($sums) {
        Write-Host "verifying checksum ..."
        $line = $sums -split "`r?`n" |
            Where-Object { $_ -match ("\s" + [regex]::Escape($Asset) + "$") } |
            Select-Object -First 1
        if (-not $line) { Die "no checksum entry for $Asset" }
        $expected = ($line -split "\s+")[0]
        $actual = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash
        if ($actual -ine $expected) { Die "checksum verification failed for $Asset" }
    } else {
        Write-Warning "SHA256SUMS not found — skipping checksum verification"
    }

    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    $exe = Join-Path $tmp "avairy.exe"
    if (-not (Test-Path $exe)) { Die "archive missing avairy.exe" }
    Copy-Item -Force -Path $exe -Destination (Join-Path $InstallDir "avairy.exe")
}
finally {
    Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
}

$target = Join-Path $InstallDir "avairy.exe"
Write-Host "installed: avairy -> $target"

# --- ensure it's on the user PATH (persisted) ------------------------------
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$onPath = $userPath -and (($userPath -split ";") -contains $InstallDir)
if (-not $onPath) {
    $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    $env:Path = "$env:Path;$InstallDir"   # this session, too
    Write-Host "added $InstallDir to your user PATH (restart your shell for new shells to see it)"
}

& $target version
