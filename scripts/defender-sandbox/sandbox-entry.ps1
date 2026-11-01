# Executed by the .wsb logon command, ONLY in the disposable guest.
$ErrorActionPreference = 'Stop'
if ($env:USERNAME -ne 'WDAGUtilityAccount') { throw 'This entry point runs only in Windows Sandbox.' }
Set-Content -LiteralPath 'C:\solu-defender-sandbox.marker' -Value 'SoluTerminal Defender qualification'
# Fixed long path: Defender resources must not depend on TEMP's 8.3 alias.
$env:TEMP = 'C:\SoluScratch'
$env:TMP = $env:TEMP
New-Item -ItemType Directory -Path $env:TEMP -Force | Out-Null
. 'C:\SoluInput\defender-scan.ps1'
$request = Get-Content -Raw 'C:\SoluInput\request.json' | ConvertFrom-Json
$report = 'C:\SoluOutput\report.json'
$since = [DateTime]::UtcNow
try {
    $null = Get-DefenderProtection
    $files = @()
    $known = @{}
    if ($request.kind -eq 'release') {
        foreach ($asset in $request.assets) {
            $path = Join-Path 'C:\SoluInput' $asset.name
            if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $asset.sha256) { throw 'Mapped input hash changed.' }
            $files += $path
        }
    } elseif ($request.kind -eq 'calibration') {
        # Samples are fetched and hashed ONLY in guest memory. Only JSON
        # evidence leaves the guest; the request explicitly opts into MAPS off.
        Invoke-DefenderCalibration $report $request.requestId ($request.requireCloudCalibration -eq $true) $request.cloudCalibrationUrl $request.cloudCalibrationSha256
        return
    } else { throw 'Unsupported Sandbox request kind.' }
    Invoke-DefenderScan $files $report $request.kind $request.requestId $known $since
} catch {
    if (-not (Test-Path -LiteralPath $report)) {
        $errorReport = @{schemaVersion=1; kind=$request.kind; requestId=$request.requestId;
            sandbox=$true; machineName=$env:COMPUTERNAME; startedAt=$since.ToString('o');
            finishedAt=[DateTime]::UtcNow.ToString('o'); status='error'; error=$_.Exception.Message; files=@()}
        Write-DefenderReport $errorReport $report
    }
}
