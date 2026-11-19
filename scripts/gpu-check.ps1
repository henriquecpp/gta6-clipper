<#
.SYNOPSIS
  Valida a GPU nos containers: CUDA sm_120 no whisper, NVENC no ffmpeg do app e timestamps por palavra.
#>
[CmdletBinding()]
param()
# Native commands report failures through $LASTEXITCODE (checked below); 'Stop' would
# turn harmless docker warnings on stderr into terminating errors in Windows PowerShell.
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path -Parent $PSScriptRoot)

Write-Host '== Driver no Windows'
& nvidia-smi --query-gpu=name,driver_version,compute_cap --format=csv,noheader

Write-Host '== whisper (CUDA sm_120)'
& docker compose run --rm --no-deps --entrypoint gpu-check whisper
if ($LASTEXITCODE -ne 0) { throw 'gpu-check do whisper falhou' }

& docker compose up -d whisper | Out-Null
Write-Host '== app (NVENC + timestamps por palavra)'
& docker compose run --rm --entrypoint clipper app gpucheck
if ($LASTEXITCODE -ne 0) { throw 'gpucheck do app falhou' }
Write-Host 'OK: GPU validada.'
