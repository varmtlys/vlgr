# Build script for Windows (PowerShell)
# Cross-compiles VLGR server and client for Windows and Linux.
#
# Usage:
#   .\scripts\build.ps1              # Build both platforms
#   .\scripts\build.ps1 -WindowsOnly # Windows only
#   .\scripts\build.ps1 -LinuxOnly   # Linux only
#
# Requires: Go 1.22+ installed and in PATH.

param(
    [switch]$WindowsOnly,
    [switch]$LinuxOnly
)

$ErrorActionPreference = "Stop"
$projectRoot = Split-Path -Parent $PSScriptRoot

$buildDir = Join-Path $projectRoot "build"
$winDir   = Join-Path $buildDir "windows"
$linuxDir = Join-Path $buildDir "linux"

$ldflags = "-s -w"   # strip debug info, shrink binary

# Timestamp for build info
$buildTime = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")

function Build-Target {
    param(
        [string]$OS,
        [string]$Arch,
        [string]$OutputDir,
        [string]$Ext
    )
    $env:GOOS   = $OS
    $env:GOARCH = $Arch
    $env:CGO_ENABLED = "0"

    New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null

    $apps = @(
        @{Name="vlgr-server"; Src=".\cmd\server"},
        @{Name="vlgr-client"; Src=".\cmd\client"}
    )

    foreach ($app in $apps) {
        $outName = "$($app.Name)$Ext"
        $outPath = Join-Path $OutputDir $outName

        Write-Host "[$OS/$Arch] Building $($app.Name)..." -ForegroundColor Cyan

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
                throw "Build failed for $($app.Name) ($OS/$Arch)"
            }
        }

        if (Test-Path $outPath) {
            $size = [math]::Round((Get-Item $outPath).Length / 1KB, 1)
            Write-Host "  -> $outName ($size KB)" -ForegroundColor Green
        }
    }
}

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

    if (-not $LinuxOnly) {
        Write-Host "--- Windows (amd64) ---" -ForegroundColor Yellow
        Build-Target -OS "windows" -Arch "amd64" -OutputDir $winDir -Ext ".exe"
        Write-Host ""
    }

    if (-not $WindowsOnly) {
        Write-Host "--- Linux (amd64) ---" -ForegroundColor Yellow
        Build-Target -OS "linux" -Arch "amd64" -OutputDir $linuxDir -Ext ""
        Write-Host ""
    }

    Write-Host "========================================" -ForegroundColor Yellow
    Write-Host "  Build complete!" -ForegroundColor Green
    Write-Host "  Output:" -ForegroundColor Yellow
    foreach ($d in @($winDir, $linuxDir)) {
        if (Test-Path $d) {
            Write-Host "    $d" -ForegroundColor Gray
            Get-ChildItem $d -File | ForEach-Object {
                $size = [math]::Round($_.Length / 1KB, 1)
                Write-Host "      $($_.Name) ($size KB)" -ForegroundColor White
            }
        }
    }
    Write-Host "========================================" -ForegroundColor Yellow
} finally {
    Pop-Location
}
