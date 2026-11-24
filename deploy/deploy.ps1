# Windows one-shot entry: build if needed, then hand over to install.ps1.
#
#   powershell -ExecutionPolicy Bypass -File .\deploy\deploy.ps1 -Origins "https://course.example.com"
#
# All parameters are forwarded to install.ps1 unchanged.
$ErrorActionPreference = "Stop"

$here = $PSScriptRoot
$exe = Join-Path $here "out\njust-proxy-windows-amd64.exe"
$releasePath = "https://github.com/qeqeq1/proxyService/releases/latest/download/njust-proxy-windows-amd64.exe"

if (-not (Test-Path $exe)) {
    # Prefer the CI-built binary: no Go toolchain on the target machine.
    # PROXY_MIRROR is exported by bootstrap.ps1; standalone runs default to official.
    $useMirror = ($env:PROXY_MIRROR -eq "cn")
    if ($useMirror) {
        $urls = @("https://ghfast.top/$releasePath", "https://gh-proxy.com/$releasePath", $releasePath)
    } else {
        $urls = @($releasePath, "https://ghfast.top/$releasePath", "https://gh-proxy.com/$releasePath")
    }
    Write-Host "==> Downloading njust-proxy-windows-amd64.exe (PROXY_MIRROR=$($env:PROXY_MIRROR))"
    New-Item -ItemType Directory -Force -Path (Join-Path $here "out") | Out-Null
    foreach ($url in $urls) {
        Write-Host "    $url"
        try {
            Invoke-WebRequest -UseBasicParsing -TimeoutSec 180 -Uri $url -OutFile $exe
            break
        } catch {
            Remove-Item $exe -ErrorAction SilentlyContinue
        }
    }
    if (-not (Test-Path $exe)) {
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
