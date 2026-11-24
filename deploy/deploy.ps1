# Windows one-shot entry: build if needed, then hand over to install.ps1.
#
#   powershell -ExecutionPolicy Bypass -File .\deploy\deploy.ps1 -Origins "https://course.example.com"
#
# All parameters are forwarded to install.ps1 unchanged.
$ErrorActionPreference = "Stop"

$here = $PSScriptRoot
$exe = Join-Path $here "out\njust-proxy-windows-amd64.exe"
$releaseUrl = "https://github.com/qeqeq1/proxyService/releases/latest/download/njust-proxy-windows-amd64.exe"

if (-not (Test-Path $exe)) {
    # Prefer the CI-built binary: no Go toolchain on the target machine.
    Write-Host "==> Downloading $releaseUrl"
    New-Item -ItemType Directory -Force -Path (Join-Path $here "out") | Out-Null
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $releaseUrl -OutFile $exe
    } catch {
        Remove-Item $exe -ErrorAction SilentlyContinue
        if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
            throw "Download failed and Go is not installed. Check that the repository has a release, or build with .\deploy\build.sh elsewhere."
        }
        Write-Host "==> Download failed, building windows/amd64 locally"
        Push-Location (Split-Path -Parent $here)
        try {
            & go build -trimpath -o $exe .
        } finally {
            Pop-Location
        }
    }
}

& (Join-Path $here "install.ps1") -Binary $exe @args
