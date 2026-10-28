<#
.SYNOPSIS
    Build the Cloudflare Tunnel .fpk package for fnOS (Windows host).

.DESCRIPTION
    Windows counterpart to build.sh, and deliberately just as thin: the pipeline
    itself lives in tools/build (Go), so the steps exist once instead of once
    per host. Both wrappers only locate the Go toolchain and hand over.

    Steps performed by tools/build: fetch the pinned cloudflared binary,
    cross-compile the service binary to linux/amd64, regenerate the icons,
    normalise the payload permissions, run the shell/UI/JSON validation, call
    fnpack, rewrite the archive permission bits (Windows has no executable bit,
    so fnpack would otherwise emit cmd/* and bin/cloudflared as 0666 and the app
    could not start), then verify the artefact against the source tree.

.PARAMETER NoIcons
    Skip icon regeneration.

.PARAMETER NoPack
    Run every validation step and stop before fnpack (hosts without fnpack).

.PARAMETER Fnpack
    Path to the fnpack executable. Defaults to $env:FNPACK, then PATH.

.PARAMETER Go
    Path to the Go executable. Defaults to PATH, then .toolchain\go\bin\go.exe.

.EXAMPLE
    pwsh -File build.ps1
    pwsh -File build.ps1 -NoIcons -Fnpack C:\tools\fnpack.exe
#>
[CmdletBinding()]
param(
    [switch]$NoIcons,
    [switch]$NoPack,
    [string]$Fnpack,
    [string]$Go
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Root = Split-Path -Parent $MyInvocation.MyCommand.Path

# ------------------------------------------------------------- tool discovery
# PATH wins; the checkout's local toolchain is the fallback so a working tree
# with .toolchain\ builds without touching the machine's PATH.
$GoExe = $Go
if (-not $GoExe) {
    $found = Get-Command go -ErrorAction SilentlyContinue
    if ($found) { $GoExe = $found.Source }
}
if (-not $GoExe) {
    $local = Join-Path $Root '.toolchain\go\bin\go.exe'
    if (Test-Path $local) { $GoExe = $local }
}
if (-not $GoExe) {
    Write-Host 'ERROR: go not found (install Go >= 1.22, add it to PATH, or pass -Go <path>)' -ForegroundColor Red
    exit 1
}
if (-not (Test-Path $GoExe)) {
    Write-Host "ERROR: not an executable: $GoExe" -ForegroundColor Red
    exit 1
}

$toolchain = Join-Path $Root '.toolchain'
if (Test-Path $toolchain) {
    if (-not $env:GOCACHE) { $env:GOCACHE = Join-Path $toolchain 'cache' }
    if (-not $env:GOMODCACHE) { $env:GOMODCACHE = Join-Path $toolchain 'mod' }
    if (-not $env:GOTMPDIR) { $env:GOTMPDIR = Join-Path $toolchain 'tmp' }
    foreach ($dir in @($env:GOCACHE, $env:GOMODCACHE, $env:GOTMPDIR)) {
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
    }
}

# tools/build shells out to `go` by name when it compiles the service binary.
$env:PATH = "$(Split-Path -Parent $GoExe);$env:PATH"

if ($Fnpack) { $env:FNPACK = $Fnpack }

$goArgs = @('run', './tools/build')
if ($NoIcons) { $goArgs += '--no-icons' }
if ($NoPack) { $goArgs += '--no-pack' }

Write-Host "go     : $GoExe"

$code = 0
Push-Location $Root
try {
    & $GoExe @goArgs
    $code = $LASTEXITCODE
}
finally {
    Pop-Location
}
if ($null -eq $code) { $code = 1 }
exit $code
