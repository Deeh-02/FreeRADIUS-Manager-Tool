# FreeRADIUS Manager - first-time setup (Windows, Docker Desktop)
# Run: PowerShell -ExecutionPolicy Bypass -File setup.ps1
# Writes .env with generated secrets (from .env.example), builds and starts the stack.
# Re-running it keeps an existing .env.
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

Write-Host "=== FreeRADIUS Manager - setup ===" -ForegroundColor Blue

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Host "[ERROR] Docker Desktop not found: https://www.docker.com/products/docker-desktop" -ForegroundColor Red
    exit 1
}
docker info *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Host "[ERROR] Docker is not running. Start Docker Desktop first." -ForegroundColor Red
    exit 1
}
docker compose version *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Host "[ERROR] 'docker compose' is not available. Update Docker Desktop." -ForegroundColor Red
    exit 1
}
Write-Host "[OK] Docker and Compose found" -ForegroundColor Green

# Cryptographic random generator (Get-Random is not one).
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
function New-Secret([int]$Length) {
    $chars = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
    $bytes = New-Object byte[] $Length
    $rng.GetBytes($bytes)
    -join ($bytes | ForEach-Object { $chars[$_ % $chars.Length] })
}
function New-Hex([int]$Bytes) {
    $b = New-Object byte[] $Bytes
    $rng.GetBytes($b)
    -join ($b | ForEach-Object { '{0:x2}' -f $_ })
}

if (Test-Path ".env") {
    Write-Host "[WARN] .env already exists; keeping it." -ForegroundColor Yellow
} else {
    $text = Get-Content ".env.example" -Raw
    $text = $text -replace '(?m)^DB_PASSWORD=.*$', "DB_PASSWORD=$(New-Secret 40)"
    $text = $text -replace '(?m)^JWT_SECRET=.*$', "JWT_SECRET=$(New-Hex 32)"
    $text = $text -replace '(?m)^MPESA_CALLBACK_TOKEN=.*$', "MPESA_CALLBACK_TOKEN=$(New-Hex 16)"
    $text = $text -replace '(?m)^RADIUS_SECRET=.*$', "RADIUS_SECRET=$(New-Secret 32)"
    $text = $text -replace '(?m)^SETUP_TOKEN=.*$', "SETUP_TOKEN=$(New-Secret 24)"
    # Containers want an IANA name (e.g. Africa/Nairobi); Windows ids differ, so UTC is
    # written unless the conversion is available.
    $tz = "UTC"
    try {
        $iana = $null
        if ([System.TimeZoneInfo]::TryConvertWindowsIdToIanaId([System.TimeZoneInfo]::Local.Id, [ref]$iana)) { $tz = $iana }
    } catch { }
    $text = $text -replace '(?m)^TZ=.*$', "TZ=$tz"
    # UTF-8 without BOM, LF line endings: Compose misreads a BOM on the first line.
    [System.IO.File]::WriteAllText((Join-Path $PWD ".env"), ($text -replace "`r`n", "`n"), (New-Object System.Text.UTF8Encoding $false))
    Write-Host "[OK] .env created with generated secrets (timezone: $tz). Edit DEFAULT_CURRENCY and SMTP there." -ForegroundColor Green
}

New-Item -ItemType Directory -Force -Path "backups" | Out-Null

Write-Host "[INFO] Building Docker images (3-10 minutes the first time)..." -ForegroundColor Cyan
docker compose build
if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] Build failed." -ForegroundColor Red; exit 1 }
Write-Host "[INFO] Starting services..." -ForegroundColor Cyan
docker compose up -d
if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] Start failed. Check: docker compose logs" -ForegroundColor Red; exit 1 }

foreach ($c in "radius_postgres", "radius_backend", "radius_freeradius", "radius_frontend") {
    $ok = $false
    for ($i = 0; $i -lt 40; $i++) {
        $s = docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' $c 2>$null
        if ($s -eq "healthy") { $ok = $true; break }
        Start-Sleep -Seconds 3
    }
    if ($ok) { Write-Host "[OK] $c is healthy" -ForegroundColor Green }
    else { Write-Host "[WARN] $c is not healthy. Check: docker logs $c" -ForegroundColor Yellow }
}

$token = (Select-String -Path ".env" -Pattern '^SETUP_TOKEN=(.*)$').Matches[0].Groups[1].Value
Write-Host ""
Write-Host "=== Setup complete ===" -ForegroundColor Green
Write-Host "  Web UI:       http://localhost:8081"
Write-Host "  RADIUS:       UDP 1812 (auth) / 1813 (accounting)"
Write-Host "  Setup token:  $token   (the setup wizard asks for it; also in .env)"
Write-Host ""
Write-Host "  Next: open the web UI, complete the setup wizard, then add your routers on the"
Write-Host "  NAS Devices page (see DEVICE-INTEGRATION.md)."
