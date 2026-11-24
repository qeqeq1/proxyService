# Windows one-shot entry: build if needed, then hand over to install.ps1.
#
#   powershell -ExecutionPolicy Bypass -File .\deploy\deploy.ps1 -Origins "https://course.example.com"
#
# All parameters are forwarded to install.ps1 unchanged.
$ErrorActionPreference = "Stop"

$here = $PSScriptRoot
$exe = Join-Path $here "out\njust-proxy-windows-amd64.exe"

if (-not (Test-Path $exe)) {
    if (Get-Command go -ErrorAction SilentlyContinue) {
        Write-Host "==> No prebuilt binary, building windows/amd64"
        Push-Location (Split-Path -Parent $here)
        try {
            & go build -trimpath -o $exe .
        } finally {
            Pop-Location
        }
    } else {
        throw "No $exe and no Go toolchain. Build on a machine with Go and copy deploy\out\ here."
    }
}

& (Join-Path $here "install.ps1") -Binary $exe @args
