<#
.SYNOPSIS
  Derruba o stack (inclui os profiles observability e test). Volumes são preservados.
#>
[CmdletBinding()]
param()
# Native commands report failures through $LASTEXITCODE (checked below); 'Stop' would
# turn harmless docker warnings on stderr into terminating errors in Windows PowerShell.
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path -Parent $PSScriptRoot)
& docker compose --profile observability --profile test down
if ($LASTEXITCODE -ne 0) { throw "docker compose down falhou ($LASTEXITCODE)" }
