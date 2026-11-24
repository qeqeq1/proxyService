# One-shot entry: no clone, no Go toolchain.
#
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy/bootstrap.ps1))) -Origins "https://a.example.com"
#
# Parameters are forwarded to deploy.ps1 unchanged.
$ErrorActionPreference = "Stop"

$base = if ($env:PROXY_BASE_URL) { $env:PROXY_BASE_URL } else { "https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy" }
$work = Join-Path $env:TEMP ("njust-proxy-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $work | Out-Null

try {
    foreach ($file in @("deploy.ps1", "install.ps1")) {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$file" -OutFile (Join-Path $work $file)
    }
    & (Join-Path $work "deploy.ps1") @args
} finally {
    Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
}
