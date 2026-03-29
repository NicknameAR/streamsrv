# StreamSrv PowerShell launcher

Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force

Write-Host "StreamSrv — loading .env..." -ForegroundColor Cyan

# ── LOAD .env ─────────────────────────────────────────────
if (Test-Path ".env") {
    Get-Content ".env" | ForEach-Object {
        $line = $_.Trim()
        if ($line -and -not $line.StartsWith("#")) {
            $idx = $line.IndexOf("=")
            if ($idx -gt 0) {
                $key = $line.Substring(0, $idx).Trim()
                $val = $line.Substring($idx + 1).Trim()
                [System.Environment]::SetEnvironmentVariable($key, $val, "Process")
                Write-Host "  $key = $val" -ForegroundColor DarkGray
            }
        }
    }
} else {
    Write-Host "[WARN] .env not found" -ForegroundColor Yellow
}

# ── PORT ──────────────────────────────────────────────────
$port = $env:PORT
if (-not $port) {
    $port = "8080"
}

# ── KILL OLD PROCESS ON PORT ─────────────────────────────
Write-Host ""
Write-Host "Cleaning port $port..." -ForegroundColor Yellow

$old = Get-NetTCPConnection -LocalPort $port -ErrorAction SilentlyContinue | Select-Object -First 1
if ($old) {
    Stop-Process -Id $old.OwningProcess -Force
    Write-Host "Old process killed" -ForegroundColor Red
}

# ── INSTALL DEPS ──────────────────────────────────────────
Write-Host ""
Write-Host "Installing deps..." -ForegroundColor Yellow
go mod tidy

# ── START SERVER ──────────────────────────────────────────
Write-Host ""
Write-Host "Starting server at http://localhost:$port" -ForegroundColor Green
Write-Host "GraphQL: http://localhost:$port/graphql" -ForegroundColor Green

# ── OPEN BROWSER ─────────────────────────────────────────
Start-Process "http://localhost:$port"

Write-Host ""
Write-Host "Press Ctrl+C to stop" -ForegroundColor DarkGray

# ── RUN WITH AUTO-RESTART ────────────────────────────────
while ($true) {
    Write-Host "Running server..." -ForegroundColor Cyan
    go run server.go graphql.go

    Write-Host ""
    Write-Host "Server crashed. Restarting in 2s..." -ForegroundColor Red
    Start-Sleep -Seconds 2
}