$ErrorActionPreference = "Stop"

# Local dev launcher for the go proxy.
# Stop with Ctrl+C. The store key lives in dev-store.key and must not be committed.

$root = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $root "bin\njust-proxy-dev.exe"
$keyFile = Join-Path $root "dev-store.key"

if (-not (Test-Path $exe)) {
    Write-Error "Missing $exe. Run: go build -trimpath -o bin/njust-proxy-dev.exe ."
}

if (-not (Test-Path $keyFile)) {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    [Convert]::ToBase64String($bytes) | Set-Content -Encoding ascii $keyFile
    Write-Host "Generated a new store key at $keyFile"
    Write-Host "Keep it: without this key the saved sessions cannot be decrypted."
}

$env:PROXY_STORE_KEY = (Get-Content $keyFile -Raw).Trim()

# Same rule as the real installer: never ship a fixed password.
$passFile = Join-Path $root "dev-admin-password.txt"
if (-not (Test-Path $passFile)) {
    $bytes = New-Object byte[] 12
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    ($bytes | ForEach-Object { $_.ToString("x2") }) -join "" | Set-Content -Encoding ascii $passFile
}
$adminPassword = (Get-Content $passFile -Raw).Trim()

Set-Location $root
Write-Host "Admin panel: http://127.0.0.1:8099/__admin__  (admin / $adminPassword)"

& $exe --host 127.0.0.1 --port 8099 `
    --allowed-targets "*.njust.edu.cn,202.119.0.0/16" `
    --allowed-origins "http://127.0.0.1:8080,http://localhost:8080" `
    --admin-user admin --admin-password $adminPassword
