# Host orchestration ONLY. There are deliberately no host Defender operations.
[CmdletBinding()]
param([string]$Config, [string]$Report,
      [Parameter(Mandatory)][string]$SandboxId, [int]$TimeoutSeconds=1200, [switch]$StopOnly)
$ErrorActionPreference = 'Stop'
if ($SandboxId -notmatch '^[0-9a-f-]{36}$') { throw 'Invalid sandbox ID' }
$wsb = Get-Command wsb.exe -ErrorAction Stop
if ($StopOnly) {
    & $wsb.Source stop --id $SandboxId
    if ($LASTEXITCODE -ne 0) { throw "Could not stop our Sandbox $SandboxId; close that guest manually." }
    return
}
if (-not $Config -or -not $Report) { throw 'Config and Report are required to start a Sandbox.' }
# Requires the Windows 11 24H2 Sandbox CLI, so stop targets only our own guest.
$started = $false
try {
    & $wsb.Source start --id $SandboxId --config (Get-Content -Raw -LiteralPath $Config)
    if ($LASTEXITCODE -ne 0) { throw 'Could not start Windows Sandbox; check the feature and wsb CLI installation.' }
    $started = $true
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while (-not (Test-Path -LiteralPath $Report)) {
        if ([DateTime]::UtcNow -ge $deadline) { throw 'Timed out waiting for Sandbox Defender JSON report.' }
        Start-Sleep -Seconds 2
    }
} finally {
    if ($started) {
        & $wsb.Source stop --id $SandboxId
        if ($LASTEXITCODE -ne 0) { throw "Could not stop our Sandbox $SandboxId; close that guest manually." }
    }
}
