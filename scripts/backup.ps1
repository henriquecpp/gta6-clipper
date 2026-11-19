<#
.SYNOPSIS
  Backup consistente do SQLite (VACUUM INTO) para .\backups\clipper-<data>.db.
#>
[CmdletBinding()]
param()
# Native commands report failures through $LASTEXITCODE (checked below); 'Stop' would
# turn harmless docker warnings on stderr into terminating errors in Windows PowerShell.
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path -Parent $PSScriptRoot)
New-Item -ItemType Directory -Force 'backups' | Out-Null
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$inContainer = "/data/backup-$stamp.db"

& docker compose exec app clipper backup $inContainer
if ($LASTEXITCODE -ne 0) { throw 'backup falhou (o app está rodando?)' }
& docker compose cp "app:$inContainer" "backups/clipper-$stamp.db"
if ($LASTEXITCODE -ne 0) { throw 'cópia do backup falhou' }
& docker compose exec app rm -f $inContainer | Out-Null
Write-Host "Backup salvo em backups\clipper-$stamp.db"
