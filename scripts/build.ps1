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
# Requires: Go 1.22+ installed and in PATH.

param(
    [string]$Targets = ""
)

$ErrorActionPreference = "Stop"
$projectRoot = Split-Path -Parent $PSScriptRoot

$buildDir = Join-Path $projectRoot "build"

$ldflags = "-s -w"   # strip debug info, shrink binary

# Timestamp for build info
$buildTime = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")

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
Write-Host "  Build time: $buildTime" -ForegroundColor Yellow
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
            }
        }
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
