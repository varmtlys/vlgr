# Build script for Windows (PowerShell)
# Cross-compiles VLGR server and client for all supported platforms.
#
# Usage:
#   .\scripts\build.ps1                                   # Build all platforms
#   .\scripts\build.ps1 -Targets "windows/amd64"          # Single platform
#   .\scripts\build.ps1 -Targets "linux/amd64,linux/arm64" # Specific platforms
#
# Available targets: windows/amd64, windows/x86, linux/amd64, linux/x86, linux/arm64, darwin/amd64, darwin/arm64
#
# Signed self-update (optional): pass an ed25519 private seed (hex) via -SignKey
# or the VLGR_SIGNING_KEY env var. The build then compiles the matching public
# key into every binary and writes a <binary>.sig alongside it. Generate a key
# once with:  go run .\scripts\sign genkey
# Without a key the binaries build unsigned and cannot self-update (fail-safe).
#
# Requires: Go 1.26+ installed and in PATH.

param(
    [string]$Targets = "",
    [string]$SignKey = $env:VLGR_SIGNING_KEY
)

$ErrorActionPreference = "Stop"
$projectRoot = Split-Path -Parent $PSScriptRoot

$buildDir = Join-Path $projectRoot "build"

# Derive version from git (tag, or commit hash if no tags).
$gitVersion = git describe --tags --always --dirty 2>$null
if ($LASTEXITCODE -ne 0 -or -not $gitVersion) { $gitVersion = "dev" }
$gitCommit = git rev-parse --short HEAD 2>$null
if ($LASTEXITCODE -ne 0 -or -not $gitCommit) { $gitCommit = "unknown" }
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$ldflags = "-s -w " +
    "-X vlgr/internal/version.Version=$gitVersion " +
    "-X vlgr/internal/version.GitCommit=$gitCommit " +
    "-X vlgr/internal/version.BuildDate=$buildDate"

# When a signing key is supplied, embed its public key so the resulting
# binaries verify their own updates. Derived on the host, before any GOOS/GOARCH
# is set for cross-compilation.
$signPubKey = ""
if ($SignKey) {
    $signPubKey = (& go run .\scripts\sign pubkey $SignKey).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $signPubKey) { throw "Failed to derive signing public key" }
    $ldflags += " -X vlgr/internal/selfupdate.SigningPublicKey=$signPubKey"
}

$allTargets = @(
    @{OS="windows"; GoArch="amd64"; Label="amd64"; Ext=".exe"},
    @{OS="windows"; GoArch="386";   Label="x86";   Ext=".exe"},
    @{OS="linux";   GoArch="amd64"; Label="amd64"; Ext=""},
    @{OS="linux";   GoArch="386";   Label="x86";   Ext=""},
    @{OS="linux";   GoArch="arm64"; Label="arm64"; Ext=""},
    @{OS="darwin";  GoArch="amd64"; Label="amd64"; Ext=""},
    @{OS="darwin";  GoArch="arm64"; Label="arm64"; Ext=""}
)

$apps = @(
    @{Name="vlgr-server"; Src=".\cmd\server"},
    @{Name="vlgr-client"; Src=".\cmd\client"}
)

Write-Host "========================================" -ForegroundColor Yellow
Write-Host "  VLGR Build Script" -ForegroundColor Yellow
Write-Host "  Version: $gitVersion ($gitCommit, $buildDate)" -ForegroundColor Yellow
Write-Host "========================================" -ForegroundColor Yellow
Write-Host ""

Push-Location $projectRoot

try {
    # Verify Go is available
    $goVer = go version 2>&1
    Write-Host "Go version: $goVer" -ForegroundColor Gray

    Write-Host "Downloading dependencies..." -ForegroundColor Cyan
    & go mod tidy 2>&1 | Out-Null
    & go mod download 2>&1 | Out-Null
    Write-Host ""

    if ($Targets) {
        $filter = $Targets -split ',' | ForEach-Object { $_.Trim() }
        $selected = $allTargets | Where-Object { "$($_.OS)/$($_.Label)" -in $filter }
        Write-Host "Building $($selected.Count) target(s): $($filter -join ', ')" -ForegroundColor Gray
    } else {
        $selected = $allTargets
        Write-Host "Building all $($selected.Count) targets" -ForegroundColor Gray
    }
    Write-Host ""

    $built = @()

    foreach ($target in $selected) {
        $os     = $target.OS
        $goArch = $target.GoArch
        $label  = $target.Label
        $ext    = $target.Ext

        Write-Host "--- $os ($label) ---" -ForegroundColor Yellow

        $suffix = if ($ext) { "-${os}-${label}${ext}" } else { "-${os}-${label}" }

        foreach ($app in $apps) {
            $outName = "$($app.Name)$suffix"
            $outPath = Join-Path $buildDir $outName

            Write-Host "[$os/$label] Building $($app.Name)..." -ForegroundColor Cyan

            $env:GOOS   = $os
            $env:GOARCH = $goArch
            $env:CGO_ENABLED = "0"

            $buildArgs = @(
                "build",
                "-trimpath",
                "-ldflags", $ldflags,
                "-o", $outPath,
                $app.Src
            )

            & go $buildArgs 2>&1 | ForEach-Object {
                if ($LASTEXITCODE -ne 0) {
                    Write-Host "  ERROR: $_" -ForegroundColor Red
                    throw "Build failed for $($app.Name) ($os/$label)"
                }
            }

            if (Test-Path $outPath) {
                $size = [math]::Round((Get-Item $outPath).Length / 1KB, 1)
                Write-Host "  -> $outName ($size KB)" -ForegroundColor Green
                $built += $outPath
            }
        }
        Write-Host ""
    }

    # Sign every artifact so the self-updater can verify it. The signing tool
    # runs on the host, so clear the cross-compilation env first.
    if ($SignKey) {
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
        Write-Host "Signing $($built.Count) binaries (public key $signPubKey)..." -ForegroundColor Cyan
        & go run .\scripts\sign sign $SignKey @built
        if ($LASTEXITCODE -ne 0) { throw "Signing failed" }
        Write-Host ""
    } else {
        Write-Host "No signing key set (VLGR_SIGNING_KEY / -SignKey): binaries are unsigned and will not self-update." -ForegroundColor DarkYellow
        Write-Host ""
    }

    Write-Host "========================================" -ForegroundColor Yellow
    Write-Host "  Build complete!" -ForegroundColor Green
    Write-Host "  Output:" -ForegroundColor Yellow
    Get-ChildItem $buildDir -File | Sort-Object Name | ForEach-Object {
        $size = [math]::Round($_.Length / 1KB, 1)
        Write-Host "    $($_.Name) ($size KB)" -ForegroundColor White
    }
    Write-Host "========================================" -ForegroundColor Yellow
} finally {
    Pop-Location
}
