# Adds Windows Defender exclusions for the self-extracting BloodHound CE build.
# Run from an elevated PowerShell:
#   powershell -ExecutionPolicy Bypass -File scripts\allow-defender.ps1 -ExePath C:\path\to\BloodHoundCE.exe
param(
    [string]$ExePath = (Join-Path $PSScriptRoot '..\dist\BloodHoundCE.exe')
)

$ErrorActionPreference = 'Stop'
$dataDir = Join-Path $env:LOCALAPPDATA 'BloodHoundCE'

Add-MpPreference -ExclusionPath $dataDir
Add-MpPreference -ExclusionPath (Resolve-Path $ExePath)
Add-MpPreference -ExclusionProcess 'bloodhound.exe'
Add-MpPreference -ExclusionProcess 'BloodHoundCE.exe'

Write-Host "Excluded: $dataDir"
Write-Host "Excluded: $((Resolve-Path $ExePath).Path)"
Write-Host "Excluded processes: bloodhound.exe, BloodHoundCE.exe"
