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

$ldflags = "-s -w"

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Ext = ".exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Ext = "" }
)

foreach ($target in $targets) {
    $env:GOOS = $target.GOOS
    $env:GOARCH = $target.GOARCH

    $exeName = "$BinaryName$($target.Ext)"
    $outPath = Join-Path $distPath $exeName

    Write-Host "Building $($target.GOOS)/$($target.GOARCH) -> $outPath" -ForegroundColor Cyan
    go build -trimpath -ldflags $ldflags -o $outPath .\cmd\server
}

Write-Host "Done." -ForegroundColor Green
