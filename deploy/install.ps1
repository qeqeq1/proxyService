# NJUST go proxy - Windows one-shot deployment (scheduled task, no third-party service wrapper)
#
#   powershell -ExecutionPolicy Bypass -File .\deploy\install.ps1
#   powershell -ExecutionPolicy Bypass -File .\deploy\install.ps1 -Port 8081 -Targets "*.njust.edu.cn,202.119.0.0/16"
#   powershell -ExecutionPolicy Bypass -File .\deploy\install.ps1 -ShowSecrets
#   powershell -ExecutionPolicy Bypass -File .\deploy\install.ps1 -Uninstall
#
# Generated layout:
#   C:\Program Files\njust-proxy\njust-proxy.exe
#   C:\ProgramData\njust-proxy\proxy.env       public config
#   C:\ProgramData\njust-proxy\secrets.env     store key + admin password (ACL: SYSTEM/Administrators)
#   C:\ProgramData\njust-proxy\data\           SQLite credential store
#   C:\ProgramData\njust-proxy\run.ps1         launcher used by the scheduled task
param(
    [int]$Port = 8081,
    [string]$Targets = "*.njust.edu.cn,202.119.0.0/16",
    [string]$Origins = "https://course.njust.store,https://course-preview.njust.store",
    [string]$AdminUser = "admin",
    [string]$Binary = "",
    [switch]$Uninstall,
    [switch]$ShowSecrets
)

$ErrorActionPreference = "Stop"
$TaskName = "njust-proxy"
$InstallDir = Join-Path $env:ProgramFiles "njust-proxy"
$ConfDir = Join-Path $env:ProgramData "njust-proxy"
$DataDir = Join-Path $ConfDir "data"
$ExePath = Join-Path $InstallDir "njust-proxy.exe"
$ProxyEnv = Join-Path $ConfDir "proxy.env"
$SecretsEnv = Join-Path $ConfDir "secrets.env"
$RunScript = Join-Path $ConfDir "run.ps1"

function Assert-Admin {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw "Run this script from an elevated PowerShell (Run as Administrator)."
    }
}

function New-StoreKey {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    return [Convert]::ToBase64String($bytes)
}

function New-AdminPassword {
    $bytes = New-Object byte[] 12
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    return ($bytes | ForEach-Object { $_.ToString("x2") }) -join ""
}

function Read-EnvValue([string]$Path, [string]$Key) {
    if (-not (Test-Path $Path)) { return "" }
    $line = Get-Content $Path | Where-Object { $_ -match "^$Key=" } | Select-Object -Last 1
    if (-not $line) { return "" }
    return $line.Substring($Key.Length + 1).Trim()
}

function Restrict-Acl([string]$Path) {
    & icacls $Path /inheritance:r /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F" | Out-Null
}

function Test-Health {
    for ($i = 0; $i -lt 20; $i++) {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 -Uri "http://127.0.0.1:$Port/__proxy__/health"
            if ($response.StatusCode -eq 200) { return $true }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    return $false
}

Assert-Admin

if ($Uninstall) {
    Write-Host "==> Removing scheduled task and binaries"
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    Stop-Process -Name "njust-proxy" -Force -ErrorAction SilentlyContinue
    Remove-Item $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host "Config and credential store kept at $ConfDir"
    Write-Host "Delete secrets.env only if you accept that all saved sessions become unreadable."
    exit 0
}

if ($ShowSecrets) {
    if (-not (Test-Path $SecretsEnv)) { throw "Not installed yet: $SecretsEnv is missing." }
    Write-Host "Admin user:     $(Read-EnvValue $ProxyEnv 'PROXY_ADMIN_USER')"
    Write-Host "Admin password: $(Read-EnvValue $SecretsEnv 'PROXY_ADMIN_PASSWORD')"
    Write-Host "Store key:      $(Read-EnvValue $SecretsEnv 'PROXY_STORE_KEY')"
    Write-Host "Back up the store key offline. Without it the credential store cannot be decrypted."
    exit 0
}

# Locate the binary: -Binary > deploy\out > current directory.
if (-not $Binary) {
    $candidates = @(
        (Join-Path $PSScriptRoot "out\njust-proxy-windows-amd64.exe"),
        (Join-Path (Get-Location) "njust-proxy-windows-amd64.exe"),
        (Join-Path (Get-Location) "njust-proxy.exe")
    )
    foreach ($candidate in $candidates) {
        if (Test-Path $candidate) { $Binary = $candidate; break }
    }
}
if (-not $Binary -or -not (Test-Path $Binary)) {
    throw "Binary not found. Run .\deploy\build.sh first, or pass -Binary <path>."
}

Write-Host "==> Preparing directories"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

$firstInstall = -not (Test-Path $SecretsEnv)
if ($firstInstall) {
    Write-Host "==> Generating store key and admin password"
    $storeKey = New-StoreKey
    $adminPassword = New-AdminPassword
    $secretLines = @(
        "# Keep this file private. Losing PROXY_STORE_KEY makes the credential store unreadable.",
        "PROXY_STORE_KEY=$storeKey",
        "PROXY_ADMIN_PASSWORD=$adminPassword"
    )
    $secretLines | Set-Content -Path $SecretsEnv -Encoding ASCII
} else {
    $adminPassword = Read-EnvValue $SecretsEnv "PROXY_ADMIN_PASSWORD"
    Write-Host "==> Reusing existing secrets (store key unchanged)"
}
Restrict-Acl $ConfDir
Restrict-Acl $SecretsEnv
Restrict-Acl $DataDir

Write-Host "==> Writing config"
$configLines = @(
    "# Target whitelist: exact origins, domain suffixes and CIDR ranges can be mixed.",
    "PROXY_ALLOWED_TARGETS=$Targets",
    "PROXY_ALLOWED_TARGET_PORTS=80,443,8080,9080",
    "PROXY_ALLOWED_ORIGINS=$Origins",
    "",
    "PROXY_STORE_FILE=$DataDir\proxy-sessions.db",
    "PROXY_SESSION_TTL=12h",
    "PROXY_CREDENTIAL_TTL=1080h",
    "PROXY_MAX_SESSIONS=5000",
    "PROXY_MAX_STORE_ENTRIES=20000",
    "PROXY_RATE_LIMIT_PER_MINUTE=120",
    "PROXY_MAX_IN_FLIGHT=64",
    "PROXY_ADMIN_USER=$AdminUser",
    "",
    "# Enable when a local nginx/Caddy terminates TLS in front of this proxy.",
    "# PROXY_TRUST_PROXY_HEADERS=1"
)
$configLines | Set-Content -Path $ProxyEnv -Encoding ASCII

Write-Host "==> Installing binary"
if (Get-Process -Name "njust-proxy" -ErrorAction SilentlyContinue) {
    Stop-Process -Name "njust-proxy" -Force
    Start-Sleep -Seconds 1
}
Copy-Item $Binary $ExePath -Force

Write-Host "==> Writing launcher"
$launcher = @"
`$ErrorActionPreference = "Stop"
Get-Content "$SecretsEnv" | Where-Object { `$_ -and -not `$_.StartsWith("#") } | ForEach-Object {
    `$name, `$value = `$_ -split "=", 2
    [Environment]::SetEnvironmentVariable(`$name, `$value, "Process")
}
Get-Content "$ProxyEnv" | Where-Object { `$_ -and -not `$_.StartsWith("#") } | ForEach-Object {
    `$name, `$value = `$_ -split "=", 2
    [Environment]::SetEnvironmentVariable(`$name, `$value, "Process")
}
& "$ExePath" --host 127.0.0.1 --port $Port
"@
$launcher | Set-Content -Path $RunScript -Encoding ASCII
Restrict-Acl $RunScript

Write-Host "==> Registering scheduled task"
$action = New-ScheduledTaskAction -Execute "powershell.exe" `
    -Argument "-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$RunScript`""
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -User "SYSTEM" -RunLevel Highest -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName

if (-not $Origins) {
    Write-Host ""
    Write-Host "WARNING: no -Origins given. The browser origin allow-list is empty," -ForegroundColor Yellow
    Write-Host "which means every request carrying an Origin header gets 403." -ForegroundColor Yellow
    Write-Host "Web frontends will not work until PROXY_ALLOWED_ORIGINS is set in $ProxyEnv." -ForegroundColor Yellow
    Write-Host "curl and native apps (no Origin header) are unaffected." -ForegroundColor Yellow
    Write-Host ""
}

Write-Host "==> Health check"
if (-not (Test-Health)) {
    Write-Host "Health check failed. Inspect: Get-ScheduledTaskInfo -TaskName $TaskName"
    throw "The proxy did not answer on http://127.0.0.1:$Port/__proxy__/health"
}

Write-Host ""
Write-Host "Deployed."
Write-Host "  Task:    schtasks /Query /TN $TaskName"
Write-Host "  Panel:   http://127.0.0.1:$Port/__admin__  (user $AdminUser, password $adminPassword)"
Write-Host "  Config:  $ProxyEnv"
Write-Host "  Secrets: $SecretsEnv  (ACL restricted to SYSTEM/Administrators)"
Write-Host "  Store:   $DataDir\proxy-sessions.db"
Write-Host ""
Write-Host "Next: put nginx/Caddy in front for TLS, set PROXY_ALLOWED_ORIGINS and"
Write-Host "PROXY_TRUST_PROXY_HEADERS=1 in proxy.env, then restart the task."
Write-Host "Back up PROXY_STORE_KEY; without it every saved session must be re-created."
if ($firstInstall) {
    Write-Host ""
    Write-Host "The password above was generated just now. Use -ShowSecrets to view it again."
}
