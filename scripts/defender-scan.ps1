# Runs ONLY inside Windows Sandbox. Only opt-in cloud calibration may toggle MAPS.
[CmdletBinding()]
param([string[]]$Files, [string]$ReportPath, [string]$Kind = 'release', [string]$RequestId)
$ErrorActionPreference = 'Stop'

function Assert-DefenderSandbox {
    if ($env:USERNAME -ne 'WDAGUtilityAccount' -or -not (Test-Path -LiteralPath 'C:\solu-defender-sandbox.marker')) {
        throw 'Refusing to scan on the host. Use scripts/defender-sandbox/launch.py.'
    }
}
function Get-DefenderProtection {
    $s = Get-MpComputerStatus -ErrorAction Stop
    $p = Get-MpPreference -ErrorAction Stop
    $cloud = [int]$p.MAPSReporting -eq 2 -and [int]$p.SubmitSamplesConsent -ne 2 -and -not $p.DisableBlockAtFirstSeen
    $protection = [ordered]@{
        engineVersion=[string]$s.AMEngineVersion; platformVersion=[string]$s.AMProductVersion;
        signatureVersion=[string]$s.AntivirusSignatureVersion;
        signatureUpdatedAt=[string]$s.AntivirusSignatureLastUpdated;
        serviceEnabled=[bool]$s.AMServiceEnabled; antivirusEnabled=[bool]$s.AntivirusEnabled;
        realTimeProtection=[bool]$s.RealTimeProtectionEnabled; cloudProtection=[bool]$cloud;
        runningMode=[string]$s.AMRunningMode; mapsReporting=[int]$p.MAPSReporting
    }
    if (@(@($p.ExclusionPath) + @($p.ExclusionProcess) + @($p.ExclusionExtension) | Where-Object { $_ }).Count) {
        throw 'Defender exclusions present; scan refused.'
    }
    if (-not $protection.serviceEnabled -or -not $protection.antivirusEnabled -or
        -not $protection.realTimeProtection -or -not $protection.cloudProtection -or
        $protection.runningMode -ne 'Normal' -or -not $protection.engineVersion -or
        -not $protection.platformVersion -or -not $protection.signatureVersion) {
        throw 'Defender must be active with real-time and cloud protection and version information; scan refused.'
    }
    return $protection
}
function Find-DefenderCli {
    $platform = Join-Path $env:ProgramData 'Microsoft/Windows Defender/Platform'
    $cli = Get-ChildItem -Path "$platform/*/MpCmdRun.exe" -ErrorAction SilentlyContinue |
        Sort-Object { [version]($_.Directory.Name -replace '-.*$', '') } -Descending |
        Select-Object -First 1 -ExpandProperty FullName
    if (-not $cli) { $cli = Join-Path $env:ProgramFiles 'Windows Defender/MpCmdRun.exe' }
    if (-not (Test-Path -LiteralPath $cli -PathType Leaf)) { throw 'MpCmdRun.exe is unavailable.' }
    return $cli
}
function Invoke-DefenderCli([string]$Cli, [string[]]$CliArguments) {
    # Windows PowerShell 5.1 converts native stderr to ErrorRecords. Collect
    # all output without turning a diagnostic line into a terminating exception.
    $previous = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $output = @(& $Cli @CliArguments 2>&1 | ForEach-Object { "$_" })
        $code = $LASTEXITCODE
    } finally { $ErrorActionPreference = $previous }
    return @{exitCode=$code; output=($output -join "`n")}
}
function Get-DefenderDetections([datetime]$Since, [string[]]$Paths) {
    $detections = @(Get-MpThreatDetection -ErrorAction Stop | Where-Object {
        $recent = $_.InitialDetectionTime.ToUniversalTime() -ge $Since.ToUniversalTime()
        $matched = $false
        foreach ($resource in $_.Resources) {
            foreach ($path in $Paths) {
                if ($path -and ([string]$resource).IndexOf($path, [StringComparison]::OrdinalIgnoreCase) -ge 0) { $matched = $true }
            }
        }
        $recent -and (-not $Paths -or $matched)
    })
    return @($detections | ForEach-Object {
        $threat = Get-MpThreat -ThreatID $_.ThreatID -ErrorAction Stop
        @{name=[string]$threat.ThreatName; resources=@($_.Resources); threatId=[string]$_.ThreatID}
    })
}
function Write-DefenderReport($Report, [string]$Path) {
    $Report.finishedAt = [DateTime]::UtcNow.ToString('o')
    $json = $Report | ConvertTo-Json -Depth 10
    $json | Set-Content -LiteralPath "$Path.tmp" -Encoding UTF8
    Move-Item -LiteralPath "$Path.tmp" -Destination $Path -Force
}
function Update-DefenderSignatures([string]$Cli) {
    for ($attempt=0; $attempt -lt 3; $attempt++) {
        $update = Invoke-DefenderCli $Cli @('-SignatureUpdate')
        if ($update.exitCode -eq 0) { return }
        if ($attempt -lt 2) { Start-Sleep -Seconds 10 }
    }
    throw 'Defender signature update failed.'
}
function Get-DefenderSampleBytes([string]$Url, [string]$ExpectedSha) {
    Assert-DefenderSandbox
    if ($Url -notmatch '^https://' -or $ExpectedSha -notmatch '^[a-f0-9]{64}$') { throw 'Sample requires HTTPS and a pinned SHA-256.' }
    $client = New-Object Net.WebClient
    try { $bytes = $client.DownloadData($Url) } finally { $client.Dispose() }
    $sha = [BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($bytes)).Replace('-', '').ToLowerInvariant()
    if ($sha -ne $ExpectedSha) { throw 'Guest sample hash mismatch.' }
    return ,$bytes
}
function Write-DefenderEicar([string]$Path) {
    Assert-DefenderSandbox
    try { Set-Content -LiteralPath $Path -Value ('X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*') -NoNewline } catch {}
}
function Invoke-DefenderSample([string]$Cli, [string]$Path, [byte[]]$Bytes, [string]$Sha, [string]$Url, [switch]$Execute) {
    Assert-DefenderSandbox
    $since = [DateTime]::UtcNow
    $entry = [ordered]@{name=[IO.Path]::GetFileName($Path); sha256=$Sha; verdict='error';
        exitCode=$null; threats=@(); detail=$null; execution=$null}
    try {
        [IO.File]::WriteAllBytes($Path, $Bytes)
        Set-Content -LiteralPath $Path -Stream Zone.Identifier -Value "[ZoneTransfer]`r`nZoneId=3`r`nHostUrl=$Url"
        Start-Sleep -Seconds 15
        $scan = Invoke-DefenderCli $Cli @('-Scan', '-ScanType', '3', '-File', $Path, '-DisableRemediation')
        $entry.exitCode = $scan.exitCode; $entry.detail = $scan.output
        if ($scan.exitCode -eq 0) { $entry.verdict='clean' }
        if ($Execute -and $entry.verdict -eq 'clean') {
            # All process output stays in guest scratch; only text in JSON leaves.
            $out = "$Path.stdout"; $err = "$Path.stderr"
            $process = Start-Process -FilePath $Path -ArgumentList '--version' -PassThru -WindowStyle Hidden -RedirectStandardOutput $out -RedirectStandardError $err
            if (-not $process.WaitForExit(15000)) {
                $process | Stop-Process -Force
                throw 'Guest sample execution timed out.'
            }
            $entry.execution = @{exitCode=$process.ExitCode; output=(Get-Content -Raw $out); error=(Get-Content -Raw $err)}
            if ($process.ExitCode -ne 0) { $entry.verdict='error' }
        }
    } catch { $entry.verdict='error'; $entry.detail=$_.Exception.Message }
    Start-Sleep -Seconds 5
    $entry.threats = @(Get-DefenderDetections $since @($Path))
    if ($entry.threats.Count) { $entry.verdict='detected' }
    return $entry
}
function Invoke-DefenderCloudCalibration([string]$Cli, [bool]$Required, [string]$Url, [string]$Sha) {
    Assert-DefenderSandbox
    $result = [ordered]@{required=$Required; calibrated=$false; scanOnly=$true; off=$null; on=$null}
    if (-not $Required) { return $result }
    # There is currently no qualifying bundled sample. Supply a pinned raw EXE
    # when one exists. Never disable real-time protection to extract a sample.
    $bytes = Get-DefenderSampleBytes $Url $Sha
    $null = Get-DefenderProtection
    try {
        Set-MpPreference -MAPSReporting Disabled -ErrorAction Stop
        Start-Sleep -Seconds 10
        $s = Get-MpComputerStatus -ErrorAction Stop; $p = Get-MpPreference -ErrorAction Stop
        if ([int]$p.MAPSReporting -ne 0 -or -not $s.RealTimeProtectionEnabled -or $s.AMRunningMode -ne 'Normal') { throw 'MAPS-off leg did not take effect with real-time protection on.' }
        # Scan only: a third-party cloud sample must never run as guest admin.
        # Off first: do not prime a cached cloud verdict before the offline leg.
        $result.off = Invoke-DefenderSample $Cli (Join-Path $env:TEMP 'cloud-off.exe') $bytes $Sha $Url
    } finally {
        Set-MpPreference -MAPSReporting Advanced -ErrorAction Stop
    }
    Start-Sleep -Seconds 10
    $null = Get-DefenderProtection
    $result.on = Invoke-DefenderSample $Cli (Join-Path $env:TEMP 'cloud-on.exe') $bytes $Sha $Url
    $result.calibrated = ($result.off.verdict -eq 'clean' -and $result.on.verdict -eq 'detected')
    return $result
}
function Invoke-DefenderCalibration([string]$Report, [string]$Id, [bool]$RequireCloud = $false, [string]$CloudUrl = '', [string]$CloudSha = '') {
    Assert-DefenderSandbox
    $result = [ordered]@{schemaVersion=2; kind='calibration'; requestId=$Id; sandbox=$true;
        machineName=$env:COMPUTERNAME; startedAt=[DateTime]::UtcNow.ToString('o'); finishedAt=$null;
        status='error'; assurance='reduced assurance: no flagged calibration sample available';
        protection=$null; protectionAfter=$null; qualification=[ordered]@{}; error=$null}
    try {
        $null = Get-DefenderProtection
        $cli = Find-DefenderCli
        Update-DefenderSignatures $cli
        $result.protection = Get-DefenderProtection
        $q = $result.qualification
        $q.maps = Invoke-DefenderCli $cli @('-ValidateMapsConnection')
        if ($q.maps.exitCode -ne 0) { throw 'MAPS connection validation failed.' }
        $eicar = Join-Path $env:TEMP 'eicar.com'; $since = [DateTime]::UtcNow
        Write-DefenderEicar $eicar
        Start-Sleep -Seconds 15
        $q.eicarThreats = @(Get-DefenderDetections $since @($eicar))
        if (-not @($q.eicarThreats | Where-Object { $_.name -match 'EICAR' }).Count) { throw 'EICAR was not caught on access.' }
        $q.cloud = Invoke-DefenderCloudCalibration $cli $RequireCloud $CloudUrl $CloudSha
        if ($RequireCloud -and -not $q.cloud.calibrated) { throw 'Cloud sample did not prove MAPS dependence.' }
        $base = 'https://github.com/soluterminal/soluterminal-releases/releases/download'
        $url = "$base/v0.0.1230/solud-windows-amd64.exe"
        $sha = 'bc0c77082d57b2fe0adcdbb1b081d7d7f41d89e9c765abf0bd8101f821e07441'
        $bytes = Get-DefenderSampleBytes $url $sha
        $q.historical = Invoke-DefenderSample $cli (Join-Path $env:TEMP 'historical-0.0.1230.exe') $bytes $sha $url -Execute
        $q.historical.informational = $true
        if ($q.historical.verdict -eq 'error') { throw 'Historical sample scan failed.' }
        $url = "$base/v0.0.1227/solud-windows-amd64.exe"
        $sha = 'c72004a764d17b383a4f2f4981c5608e7e628ccf2193e09162fc0056e79876dd'
        $bytes = Get-DefenderSampleBytes $url $sha
        $q.control = Invoke-DefenderSample $cli (Join-Path $env:TEMP 'control-0.0.1227.exe') $bytes $sha $url
        if ($q.control.verdict -ne 'clean') { throw 'Pinned clean control was not clean.' }
        $result.status='qualified'
    } catch { $result.error=$_.Exception.Message }
    finally {
        # Even a failure must record the final state. Never silently bless a
        # run whose optional MAPS leg failed to restore full protection.
        try { $result.protectionAfter=Get-DefenderProtection }
        catch { $result.error=$_.Exception.Message; $result.status='error' }
        Write-DefenderReport $result $Report
    }
    if ($result.status -ne 'qualified') { throw "Defender calibration failed. See $Report" }
}
function Invoke-DefenderFileScan([string]$Cli, [string]$Path, [datetime]$Since) {
    $entry = [ordered]@{name=[IO.Path]::GetFileName($Path); sha256=$null; verdict='error'; exitCode=$null; threats=@(); detail=$null}
    try {
        $entry.sha256 = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
        $scan = Invoke-DefenderCli $Cli @('-Scan', '-ScanType', '3', '-File', $Path, '-DisableRemediation')
        $entry.exitCode=$scan.exitCode; $entry.detail=$scan.output
        if ($scan.exitCode -eq 0) { $entry.verdict='clean' }
    } catch { $entry.detail=$_.Exception.Message }
    $entry.threats = @(Get-DefenderDetections $Since @($Path))
    if ($entry.threats.Count) { $entry.verdict='detected' }
    return $entry
}
function Expand-DefenderInstaller([string]$Installer, [string]$Destination) {
    Assert-DefenderSandbox
    $sevenZip = 'C:\SoluInput\7z\7z.exe'
    $extracted = Invoke-DefenderCli $sevenZip @('x', '-y', "-o$Destination", $Installer)
    if ($extracted.exitCode -ne 0) { throw "7-Zip extraction failed: $($extracted.output)" }
}
function Invoke-DefenderProcess([string]$Path, [string[]]$Arguments, [int]$TimeoutSeconds, [switch]$Observe) {
    Assert-DefenderSandbox
    $out = Join-Path $env:TEMP ([Guid]::NewGuid().ToString() + '.stdout')
    $err = "$out.stderr"
    $options = @{FilePath=$Path; PassThru=$true; RedirectStandardOutput=$out; RedirectStandardError=$err}
    if ($Arguments.Count) { $options.ArgumentList=$Arguments }
    $process = Start-Process @options
    try {
        # PS 5.1 can lose ExitCode for a fast process unless its handle is cached.
        $null = $process.Handle
        if ($Observe) {
            Start-Sleep -Seconds $TimeoutSeconds
            $process.Refresh()
            if ($process.HasExited -and $process.ExitCode -ne 0) { throw 'Desktop exited with an error.' }
            return @{started=$true; observedSeconds=$TimeoutSeconds; exitCode=$(if ($process.HasExited) {$process.ExitCode} else {$null})}
        }
        if (-not $process.WaitForExit($TimeoutSeconds * 1000)) { throw "Process timed out: $Path" }
        # Flush async process state before reading ExitCode in Windows PS 5.1.
        $process.WaitForExit()
        return @{exitCode=$process.ExitCode; output=(Get-Content -Raw $out); error=(Get-Content -Raw $err)}
    } finally {
        if (-not $process.HasExited) { $process | Stop-Process -Force }
    }
}
function Invoke-DefenderInstaller([string]$Cli, [string]$Installer, [datetime]$Since) {
    Assert-DefenderSandbox
    $result = [ordered]@{name=[IO.Path]::GetFileName($Installer); sha256=$null; contents=@(); install=$null; daemon=$null; desktop=$null; status='error'; error=$null}
    try {
        $result.sha256 = (Get-FileHash -LiteralPath $Installer -Algorithm SHA256).Hash.ToLowerInvariant()
        $expanded = Join-Path $env:TEMP ([Guid]::NewGuid().ToString() + '-nsis')
        Expand-DefenderInstaller $Installer $expanded
        $exes = @(Get-ChildItem -LiteralPath $expanded -Recurse -File -Filter '*.exe')
        if ($exes.Count -lt 2) { throw 'Installer extraction found fewer than two executables.' }
        foreach ($exe in $exes) {
            $entry = Invoke-DefenderFileScan $Cli $exe.FullName $Since
            $entry.name = $exe.FullName.Substring($expanded.Length + 1)
            $result.contents += $entry
        }
        if (@($result.contents | Where-Object { $_.verdict -ne 'clean' }).Count) { throw 'Installer content was not clean.' }
        $result.install = Invoke-DefenderProcess $Installer @('/S') 180
        if ($result.install.exitCode -ne 0) { throw 'Silent install failed.' }
        Start-Sleep -Seconds 20
        $installDir = Join-Path $env:LOCALAPPDATA 'soluterminal'
        $result.daemon = Invoke-DefenderProcess (Join-Path $installDir 'solud.exe') @('--version') 15
        if ($result.daemon.exitCode -ne 0) { throw 'Installed solud --version failed.' }
        $result.desktop = Invoke-DefenderProcess (Join-Path $installDir 'soluterminal-desktop.exe') @() 15 -Observe
        $result.status='clean'
    } catch { $result.error=$_.Exception.Message }
    return $result
}
function Invoke-DefenderScan([string[]]$ScanFiles, [string]$Report, [string]$ReportKind, [string]$Id, [hashtable]$KnownHashes = @{}, [datetime]$EvidenceSince = [DateTime]::UtcNow) {
    Assert-DefenderSandbox
    if (-not $Report -or -not $ScanFiles -or $ScanFiles.Count -eq 0) { throw 'Files and ReportPath are required.' }
    $started = [DateTime]::UtcNow
    $result = [ordered]@{schemaVersion=2; kind=$ReportKind; requestId=$Id; sandbox=$true;
        machineName=$env:COMPUTERNAME; startedAt=$started.ToString('o'); finishedAt=$null;
        status='error'; protection=$null; protectionAfter=$null; files=@(); installers=@(); unexpectedDetections=@(); error=$null}
    try {
        $null = Get-DefenderProtection
        $cli = Find-DefenderCli
        Update-DefenderSignatures $cli
        $result.protection = Get-DefenderProtection
        foreach ($file in $ScanFiles) {
            $entry = [ordered]@{name=[IO.Path]::GetFileName($file); sha256=$null; verdict='error'; exitCode=$null; threats=@(); detail=$null}
            $scratch = $null
            try {
                if (Test-Path -LiteralPath $file -PathType Leaf) {
                    # Local sandbox scratch, so remediation can never write to mapped input.
                    $scratch = Join-Path $env:TEMP ([Guid]::NewGuid().ToString() + '.exe')
                    Copy-Item -LiteralPath $file -Destination $scratch
                    $entry.sha256 = (Get-FileHash -LiteralPath $scratch -Algorithm SHA256).Hash.ToLowerInvariant()
                } elseif ($KnownHashes.ContainsKey($file)) {
                    # Guest downloader hashed the bytes in memory before writing. A
                    # real-time quarantine may remove the file before on-demand scan.
                    $entry.sha256 = $KnownHashes[$file]
                    throw 'File disappeared after guest download (possible real-time quarantine).'
                } else { throw "Input file missing: $file" }
                $scan = Invoke-DefenderCli $cli @('-Scan', '-ScanType', '3', '-File', $scratch, '-DisableRemediation')
                $entry.exitCode = $scan.exitCode
                $entry.detail = $scan.output
                $entry.threats = @(Get-DefenderDetections $EvidenceSince @($file, $scratch))
                if ($scan.exitCode -eq 0 -and $entry.threats.Count -eq 0) { $entry.verdict='clean' }
                elseif ($entry.threats.Count -gt 0) { $entry.verdict='detected' }
            } catch {
                $entry.detail = $_.Exception.Message
                $entry.threats = @(Get-DefenderDetections $EvidenceSince @($file, $scratch))
                if ($entry.threats.Count -gt 0) { $entry.verdict='detected' }
            }
            $result.files += $entry
            if ([IO.Path]::GetFileName($file) -like '*-setup.exe' -and $entry.verdict -eq 'clean') {
                # Keep using the same hashed guest copy for extraction and installation.
                $installerResult = Invoke-DefenderInstaller $cli $scratch $EvidenceSince
                $installerResult.name = $entry.name
                $result.installers += $installerResult
            }
        }
        if ($ReportKind -eq 'release' -and $result.installers.Count -ne 1) { throw 'Exactly one clean installer is required.' }
        $result.protectionAfter = Get-DefenderProtection
        if (@($result.installers | Where-Object { $_.status -ne 'clean' }).Count) { throw 'Installer coverage failed.' }
        if (@($result.files | Where-Object { $_.verdict -ne 'clean' }).Count -eq 0) { $result.status='clean' }
        elseif (@($result.files | Where-Object { $_.verdict -eq 'detected' }).Count -gt 0) { $result.status='detected' }
    } catch { $result.error = $_.Exception.Message }
    finally {
        # Qualification is a separate guest/run: there are no permitted sample
        # paths in a release scan. Include ALL detections, including installed
        # files, child processes and detections with no resources.
        try {
            Start-Sleep -Seconds 20
            $result.unexpectedDetections = @(Get-DefenderDetections $EvidenceSince @())
            if ($result.unexpectedDetections.Count) { $result.status='detected' }
            $result.protectionAfter = Get-DefenderProtection
        } catch { $result.status='error'; $result.error=$_.Exception.Message }
        Write-DefenderReport $result $Report
    }
    if ($result.status -ne 'clean') { throw "Defender result: $($result.status). See $Report" }
}
if ($MyInvocation.InvocationName -ne '.') { Invoke-DefenderScan $Files $ReportPath $Kind $RequestId }
