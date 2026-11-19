<#
.SYNOPSIS
  Sobe o stack (app + whisper). Use -Obs para incluir Prometheus e Grafana.
.EXAMPLE
  .\scripts\up.ps1 -Obs
#>
[CmdletBinding()]
param(
  [switch]$Obs,
  [switch]$Build
)
# Native commands report failures through $LASTEXITCODE (checked below); 'Stop' would
# turn harmless docker warnings on stderr into terminating errors in Windows PowerShell.
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path -Parent $PSScriptRoot)

if (-not (Test-Path 'config.yaml')) {
  Copy-Item 'config.example.yaml' 'config.yaml'
  Write-Host 'config.yaml criado a partir de config.example.yaml — edite a allowlist de streamers.'
}
if (-not (Test-Path '.env')) {
  Write-Warning '.env não encontrado: o app sobe em modo degradado (sem Twitch/Telegram/Claude). Copie .env.example para .env.'
}
New-Item -ItemType Directory -Force 'output' | Out-Null

$dcArgs = @('compose')
if ($Obs) { $dcArgs += @('--profile', 'observability') }
$dcArgs += @('up', '-d')
if ($Build) { $dcArgs += '--build' }
& docker @dcArgs
if ($LASTEXITCODE -ne 0) { throw "docker compose up falhou ($LASTEXITCODE)" }

Write-Host 'App:      http://localhost:8080/healthz'
if ($Obs) {
  Write-Host 'Grafana:  http://localhost:3000 (dashboard gta6-clipper)'
  Write-Host 'Prometheus: http://localhost:9090'
}
