<#
.SYNOPSIS
  Acompanha os logs do stack. -Service app|whisper|prometheus|grafana; -Tail N linhas.
.EXAMPLE
  .\scripts\logs.ps1 -Service app
#>
[CmdletBinding()]
param(
  [string]$Service = '',
  [int]$Tail = 200
)
# Native commands report failures through $LASTEXITCODE (checked below); 'Stop' would
# turn harmless docker warnings on stderr into terminating errors in Windows PowerShell.
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path -Parent $PSScriptRoot)
$dcArgs = @('compose', '--profile', 'observability', 'logs', '-f', "--tail=$Tail")
if ($Service) { $dcArgs += $Service }
& docker @dcArgs
