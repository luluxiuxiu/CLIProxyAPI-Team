param(
    [string]$OutputDir = "dist",
    [string]$BinaryName = "cli-proxy-api"
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $repoRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go is not installed or not in PATH."
}

$distPath = Join-Path $repoRoot $OutputDir
if (-not (Test-Path $distPath)) {
    New-Item -ItemType Directory -Path $distPath | Out-Null
}

$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"

$exeName = "$BinaryName.exe"
$outPath = Join-Path $distPath $exeName

Write-Host "Building $exeName -> $outPath" -ForegroundColor Cyan

$ldflags = "-s -w"

go build -trimpath -ldflags $ldflags -o $outPath .\cmd\server

Write-Host "Done." -ForegroundColor Green
