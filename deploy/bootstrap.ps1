# One-shot entry: no clone, no Go toolchain.
#
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy/bootstrap.ps1))) -Origins "https://a.example.com"
#
# Mainland China networks switch to a mirror automatically. Override with:
#   $env:PROXY_MIRROR = "cn"      force mirror
#   $env:PROXY_MIRROR = "github"  force official
#   $env:PROXY_MIRROR = "auto"    default
$ErrorActionPreference = "Stop"

$mirror = if ($env:PROXY_MIRROR) { $env:PROXY_MIRROR } else { "auto" }
$useMirror = $mirror -eq "cn"

if ($mirror -eq "auto") {
    try {
        $country = (Invoke-RestMethod -UseBasicParsing -TimeoutSec 3 -Uri "https://ipinfo.io/country").Trim()
        if ($country -eq "CN") {
            $useMirror = $true
            if ([Environment]::UserInteractive) {
                $answer = Read-Host "Detected a mainland China network. Use the mirror for downloads? [Y/n]"
                if ($answer -match "^[Nn]") { $useMirror = $false }
            }
        }
    } catch {
        # Country lookup failed: keep the official source, the downloader will still fall back.
    }
}
if ($useMirror) { $env:PROXY_MIRROR = "cn" }

function Get-DeployFile([string]$relative, [string]$outFile) {
    $github = "https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy/$relative"
    $jsdelivr = "https://cdn.jsdelivr.net/gh/qeqeq1/proxyService@main/deploy/$relative"
    if ($useMirror) {
        $urls = @("https://gh-proxy.com/$github", "https://ghfast.top/$github", $jsdelivr, $github)
    } else {
        $urls = @($github, "https://gh-proxy.com/$github", "https://ghfast.top/$github")
    }
    foreach ($url in $urls) {
        try {
            Invoke-WebRequest -UseBasicParsing -TimeoutSec 60 -Uri $url -OutFile $outFile
            return
        } catch {
            Remove-Item $outFile -ErrorAction SilentlyContinue
        }
    }
    throw "Failed to download $relative (tried $($urls -join ', '))."
}

$work = Join-Path $env:TEMP ("njust-proxy-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $work | Out-Null

try {
    foreach ($file in @("deploy.ps1", "install.ps1")) {
        Get-DeployFile $file (Join-Path $work $file)
    }
    & (Join-Path $work "deploy.ps1") @args
} finally {
    Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
}
