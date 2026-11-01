# Offline tests only. All Defender and sandbox checks below are mocked.
param([string]$FixtureDirectory)
$ErrorActionPreference = 'Stop'
if ($FixtureDirectory -and ($PSVersionTable.PSVersion.Major -ne 5 -or $PSVersionTable.PSVersion.Minor -ne 1)) { throw 'Fixtures must exercise Windows PowerShell 5.1 serialization' }
. "$PSScriptRoot/defender-scan.ps1"
function Must-Fail([scriptblock]$Action) {
    $failed=$false
    try { & $Action } catch { $failed=$true }
    if (-not $failed) { throw 'Expected scan refusal' }
}
# The public entry point must reject this non-Sandbox host before a scan.
if ($env:USERNAME -ne 'WDAGUtilityAccount') {
    Must-Fail { Assert-DefenderSandbox }
}
# Exercise the real stream collector with an inert child process, never Defender.
$nativeInvoker = ${function:Invoke-DefenderCli}
$native = & $nativeInvoker (Get-Process -Id $PID).Path @('-NoProfile', '-NonInteractive', '-Command', "[Console]::Error.WriteLine('diagnostic'); exit 2")
if ($native.exitCode -ne 2 -or $native.output -notmatch 'diagnostic') { throw 'Native stderr or exit code was lost' }
$readDetections = ${function:Get-DefenderDetections}
function Get-MpThreatDetection {
    [pscustomobject]@{InitialDetectionTime=[DateTime]::Now.AddSeconds(-1); Resources=@('file:_C:\guest\expected.exe'); ThreatID=1}
    [pscustomobject]@{InitialDetectionTime=[DateTime]::Now.AddSeconds(-1); Resources=@('file:_C:\guest\unrelated.exe'); ThreatID=2}
}
function Get-MpThreat { param($ThreatID) [pscustomobject]@{ThreatName="fixture-$ThreatID"} }
$detected = @(& $readDetections ([DateTime]::UtcNow.AddMinutes(-1)) @('C:\guest\expected.exe'))
if ($detected.Count -ne 1 -or $detected[0].name -ne 'fixture-1') { throw 'Detections must match the actual file path' }
if (@(& $readDetections ([DateTime]::UtcNow.AddMinutes(-1)) @()).Count -ne 2) { throw 'Global detection collection omitted unrelated paths' }
function Assert-DefenderSandbox {}
$script:enabled=$true; $script:cloud=$true; $script:mode='Normal'; $script:maps=2; $script:realtime=$true; $script:exclusions=@()
$script:scanExit=0; $script:updateExit=0; $script:threats=@(); $script:calls=@()
function Get-MpComputerStatus {
    [pscustomobject]@{ AMServiceEnabled=$script:enabled; AntivirusEnabled=$true;
        RealTimeProtectionEnabled=$script:realtime; AMRunningMode=$script:mode; AMEngineVersion='1.2.3';
        AMProductVersion='4.5.6'; AntivirusSignatureVersion='7.8.9' }
}
function Get-MpPreference {
    [pscustomobject]@{MAPSReporting=$(if ($script:cloud) {$script:maps} else {0}); SubmitSamplesConsent=1; DisableBlockAtFirstSeen=$false; ExclusionPath=$script:exclusions}
}
function Find-DefenderCli { 'mock-cli' }
function Get-DefenderDetections { return $script:threats }
function Start-Sleep {}
function Invoke-DefenderCli([string]$Cli, [string[]]$CliArguments) {
    $script:calls += ,@($CliArguments)
    if ($CliArguments[0] -eq '-SignatureUpdate') { return @{exitCode=$script:updateExit; output='mock update'} }
    return @{exitCode=$script:scanExit; output='mock scan'}
}
$temp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $temp | Out-Null
$oldTemp=$env:TEMP; $env:TEMP=$temp
$oldMachine=$env:COMPUTERNAME; $env:COMPUTERNAME='SandboxFixture'
$fixture=Join-Path $temp 'inert.exe'; Set-Content -LiteralPath $fixture -Value 'inert fixture'
$report=Join-Path $temp 'report.json'
try {
    Invoke-DefenderScan @($fixture) $report 'fixture' 'test'
    $r=Get-Content -Raw $report | ConvertFrom-Json
    if ($r.status -ne 'clean' -or $r.files[0].sha256.Length -ne 64 -or -not $r.protection.cloudProtection) { throw 'Incomplete clean report' }
    $scan=$script:calls[1]
    if ($scan[0] -ne '-Scan' -or $scan[-1] -ne '-DisableRemediation' -or $scan[4] -eq $fixture) { throw 'Scan must target guest scratch with no remediation' }
    $script:cloud=$false
    Must-Fail { Invoke-DefenderScan @($fixture) $report 'fixture' 'test' }
    if ((Get-Content -Raw $report | ConvertFrom-Json).status -ne 'error') { throw 'Cloud-disabled scan must write error report' }
    $script:cloud=$true; $script:enabled=$false
    Must-Fail { Invoke-DefenderScan @($fixture) $report 'fixture' 'test' }
    $script:enabled=$true; $script:mode='Passive'
    Must-Fail { Invoke-DefenderScan @($fixture) $report 'fixture' 'test' }
    $script:mode='Normal'; $script:updateExit=2
    Must-Fail { Invoke-DefenderScan @($fixture) $report 'fixture' 'test' }
    $script:updateExit=0; $script:scanExit=2
    Must-Fail { Invoke-DefenderScan @($fixture) $report 'fixture' 'test' }
    if ((Get-Content -Raw $report | ConvertFrom-Json).files[0].verdict -ne 'error') { throw 'Scanner errors are not clean or detections without evidence' }
    $script:threats=@(@{name='Trojan:Win32/Bearfoos.A!ml'; resources=@($fixture)})
    Must-Fail { Invoke-DefenderScan @($fixture) $report 'calibration' 'test' }
    if ((Get-Content -Raw $report | ConvertFrom-Json).files[0].verdict -ne 'detected') { throw 'Detection missing' }
    $script:threats=@(); $script:scanExit=0
    Must-Fail { Invoke-DefenderScan @("$fixture-missing") $report 'fixture' 'test' }
    # Exercise actual calibration sample scanning with inert bytes only.
    $sampleScanner = ${function:Invoke-DefenderSample}
    $samplePath = Join-Path $temp 'sample.exe'
    $sample = & $sampleScanner 'mock-cli' $samplePath ([byte[]](1,2,3)) ('a'*64) 'https://example.test/inert.exe'
    if ($sample.verdict -ne 'clean' -or $sample.exitCode -ne 0) { throw 'Sample clean scan evidence missing' }
    $script:scanExit=2
    $sample = & $sampleScanner 'mock-cli' $samplePath ([byte[]](1,2,3)) ('a'*64) 'https://example.test/inert.exe'
    if ($sample.verdict -ne 'error') { throw 'Sample scan errors must fail closed' }
    $script:scanExit=0
    $script:threats=@(@{name='MockDetection'})
    $sample = & $sampleScanner 'mock-cli' $samplePath ([byte[]](1,2,3)) ('a'*64) 'https://example.test/inert.exe'
    if ($sample.verdict -ne 'detected') { throw 'On-access sample detection missing' }
    $script:threats=@()

    # Exercise the real PS 5.1 process helper with inert OS commands too:
    # fast exit codes and timeout cleanup must work without orchestration mocks.
    $cmd = Join-Path $env:SystemRoot 'System32/cmd.exe'
    $fast = Invoke-DefenderProcess $cmd @('/c', 'echo fixture version') 15
    if ($fast.exitCode -isnot [int] -or $fast.exitCode -ne 0 -or $fast.output -notmatch 'fixture version') { throw 'Fast process exit code/output missing' }
    $failed = Invoke-DefenderProcess $cmd @('/c', 'exit 7') 15
    if ($failed.exitCode -ne 7) { throw 'Nonzero process exit code missing' }
    $powershell = (Get-Process -Id $PID).Path
    Must-Fail { Invoke-DefenderProcess $powershell @('-NoProfile', '-NonInteractive', '-Command', 'Start-Sleep -Seconds 5') 0 }
    # Exercise both real cloud legs with inert bytes and no process launch.
    function Get-DefenderSampleBytes { return ,([byte[]](1,2,3)) }
    function Set-MpPreference([string]$MAPSReporting) { $script:maps = if ($MAPSReporting -eq 'Disabled') {0} else {2} }
    function Start-Process { throw 'Cloud legs must never launch a process' }
    $cloudResult=Invoke-DefenderCloudCalibration 'mock-cli' $true 'https://example.test/inert.exe' ('a'*64)
    if ($cloudResult.off.verdict -ne 'clean' -or $cloudResult.on.verdict -ne 'clean' -or $cloudResult.off.execution -or $cloudResult.on.execution) { throw 'Cloud legs did not use scan-only sample path' }
    if ((Get-Content -LiteralPath (Join-Path $temp 'cloud-off.exe') -Stream Zone.Identifier) -notcontains 'ZoneId=3') { throw 'Cloud sample Mark of the Web missing' }
    # Full qualification orchestration. No downloads, malware, real scans or
    # real settings changes: only these mocks may service the guest helpers.
    $script:eicar=$true; $script:mapsExit=0; $script:historical='clean'; $script:control='clean'
    $script:cloudOff='clean'; $script:cloudOn='detected'; $script:settings=@(); $script:samples=@(); $script:downloadFailure=$false; $script:failAfter=$false; $script:restoreFails=$false
    function Write-DefenderEicar([string]$Path) {} # Never write even EICAR to the host.
    function Get-DefenderSampleBytes([string]$Url, [string]$Sha) {
        if ($script:downloadFailure) { throw 'Mock hash mismatch' }
        return ,([byte[]](1,2,3))
    }
    function Set-MpPreference([string]$MAPSReporting) {
        $script:settings += $MAPSReporting
        if ($MAPSReporting -eq 'Disabled') { $script:maps=0 }
        elseif ($MAPSReporting -eq 'Advanced') { if (-not $script:restoreFails) { $script:maps=2 } }
        else { throw 'Unexpected settings mutation' }
    }
    function Get-DefenderDetections([datetime]$Since, [string[]]$Paths) {
        if ($script:eicar -and $Paths[0] -like '*eicar.com') { return @(@{name='Virus:DOS/EICAR_Test_File'}) }
        return @()
    }
    function Invoke-DefenderCli([string]$Cli, [string[]]$CliArguments) {
        $script:calls += ,@($CliArguments)
        $code = if ($CliArguments[0] -eq '-ValidateMapsConnection') { $script:mapsExit } else { 0 }
        return @{exitCode=$code; output='mock'}
    }
    function Invoke-DefenderSample([string]$Cli, [string]$Path, [byte[]]$Bytes, [string]$Sha, [string]$Url, [switch]$Execute) {
        $script:samples += [IO.Path]::GetFileName($Path)
        if ($Path -like '*cloud-*' -and $Execute) { throw 'Third-party cloud sample must never execute' }
        if ($Path -like '*historical*' -and -not $Execute) { throw 'Historical execution coverage was removed' }
        if ($script:failAfter -and $Path -like '*control*') { $script:realtime=$false }
        $v = if ($Path -like '*historical*') { $script:historical } elseif ($Path -like '*control*') { $script:control } elseif ($script:maps -eq 0) { $script:cloudOff } else { $script:cloudOn }
        $sample = [ordered]@{sha256=$Sha; verdict=$v; exitCode=$(if ($v -eq 'clean') {0} else {2}); threats=@(); execution=$null}
        if ($v -eq 'detected') { $sample.threats = @(@{name='MockDetection'; resources=@($Path)}) }
        return $sample
    }
    Invoke-DefenderCalibration $report 'test'
    $r=Get-Content -Raw $report | ConvertFrom-Json
    if ($r.schemaVersion -ne 2 -or $r.status -ne 'qualified' -or $r.qualification.historical.verdict -ne 'clean' -or -not $r.qualification.historical.informational -or $r.qualification.control.sha256 -ne 'c72004a764d17b383a4f2f4981c5608e7e628ccf2193e09162fc0056e79876dd' -or $r.assurance -ne 'reduced assurance: no flagged calibration sample available') { throw 'Reduced-assurance qualification incomplete' }
    if ($script:settings.Count -or @($script:samples | Where-Object { $_ -like 'cloud-*' }).Count) { throw 'Default qualification changed settings or ran a cloud sample' }
    if ($FixtureDirectory) { New-Item -ItemType Directory -Path $FixtureDirectory -Force | Out-Null; Copy-Item $report (Join-Path $FixtureDirectory 'calibration.json') }
    $script:historical='detected'; Invoke-DefenderCalibration $report 'test'
    $script:historical='error'; Must-Fail { Invoke-DefenderCalibration $report 'test' }; $script:historical='clean'
    $script:eicar=$false; Must-Fail { Invoke-DefenderCalibration $report 'test' }; $script:eicar=$true
    $script:mapsExit=2; Must-Fail { Invoke-DefenderCalibration $report 'test' }; $script:mapsExit=0
    $script:control='detected'; Must-Fail { Invoke-DefenderCalibration $report 'test' }; $script:control='clean'
    $script:downloadFailure=$true; Must-Fail { Invoke-DefenderCalibration $report 'test' }; $script:downloadFailure=$false
    $script:realtime=$false; Must-Fail { Invoke-DefenderCalibration $report 'test' }; $script:realtime=$true
    $script:exclusions=@('C:\'); Must-Fail { Invoke-DefenderCalibration $report 'test' }; $script:exclusions=@()
    $script:failAfter=$true; Must-Fail { Invoke-DefenderCalibration $report 'test' }
    if ((Get-Content -Raw $report | ConvertFrom-Json).status -ne 'error') { throw 'Final protection failure must not qualify' }
    $script:failAfter=$false; $script:realtime=$true
    Invoke-DefenderCalibration $report 'test' $true 'https://example.test/cloud.exe' ('a'*64)
    $r=Get-Content -Raw $report | ConvertFrom-Json
    if (-not $r.qualification.cloud.calibrated -or $r.protectionAfter.mapsReporting -ne 2 -or ($script:settings -join ',') -ne 'Disabled,Advanced') { throw 'Cloud comparison must be off first and restore MAPS Advanced' }
    if ($FixtureDirectory) { Copy-Item $report (Join-Path $FixtureDirectory 'cloud.json') }
    $script:cloudOff='detected'
    Must-Fail { Invoke-DefenderCalibration $report 'test' $true 'https://example.test/cloud.exe' ('a'*64) }
    if ($script:maps -ne 2) { throw 'MAPS not restored after failed calibration' }
    $script:cloudOff='clean'; $script:cloudOn='clean'
    Must-Fail { Invoke-DefenderCalibration $report 'test' $true 'https://example.test/cloud.exe' ('a'*64) }
    $script:cloudOn='detected'
    $script:restoreFails=$true
    Must-Fail { Invoke-DefenderCalibration $report 'test' $true 'https://example.test/cloud.exe' ('a'*64) }
    if ((Get-Content -Raw $report | ConvertFrom-Json).status -ne 'error') { throw 'MAPS restoration failure must not qualify' }
    $script:restoreFails=$false; $script:maps=2
    # A terminating error in the off leg must still restore MAPS.
    function Invoke-DefenderSample { throw 'Mock scan exception' }
    Must-Fail { Invoke-DefenderCalibration $report 'test' $true 'https://example.test/cloud.exe' ('a'*64) }
    $r=Get-Content -Raw $report | ConvertFrom-Json
    if ($script:maps -ne 2 -or $r.status -ne 'error' -or $r.protectionAfter.mapsReporting -ne 2) { throw 'Failure did not record restored protection' }
    # Real release/installer/report paths, inert files and mocked external operations.
    $script:processCalls=@(); $script:extractFails=$false; $script:processFails=''; $script:innerDetection=$false; $script:globalDetection=$false
    function Expand-DefenderInstaller([string]$Installer, [string]$Destination) {
        if ($script:extractFails) { throw 'mock extraction failure' }
        New-Item -ItemType Directory -Path $Destination -Force | Out-Null
        Set-Content (Join-Path $Destination 'solud.exe') 'inert daemon'
        Set-Content (Join-Path $Destination 'soluterminal-desktop.exe') 'inert desktop'
    }
    function Invoke-DefenderProcess([string]$Path, [string[]]$Arguments, [int]$TimeoutSeconds, [switch]$Observe) {
        $script:processCalls += @{path=$Path; arguments=$Arguments; timeout=$TimeoutSeconds; observe=[bool]$Observe}
        if ($script:processFails -and ($Path -like "*$script:processFails*" -or $Arguments -contains $script:processFails)) { throw 'mock process failure' }
        if ($Observe) { return @{started=$true; observedSeconds=$TimeoutSeconds; exitCode=$null} }
        return @{exitCode=0; output='fixture version'; error=''}
    }
    function Get-DefenderDetections([datetime]$Since, [string[]]$Paths) {
        if (($script:globalDetection -and -not $Paths) -or ($script:innerDetection -and $Paths[0] -like '*-nsis*')) {
            return @{name='MockUnexpected'; resources=@('file:_C:\unrelated\child.exe')}
        }
        return @()
    }
    $setup=Join-Path $temp 'soluterminal_1.2.3_x64-setup.exe'; Set-Content $setup 'inert setup'
    $daemon=Join-Path $temp 'solud-windows-amd64.exe'; Set-Content $daemon 'inert daemon'
    Invoke-DefenderScan @($daemon, $setup) $report 'release' 'test'
    $r=Get-Content -Raw $report | ConvertFrom-Json
    if ($r.schemaVersion -ne 2 -or $r.installers.Count -ne 1 -or $r.installers[0].contents.Count -ne 2 -or $r.unexpectedDetections.Count) { throw 'Installer coverage incomplete' }
    if ($script:processCalls.Count -ne 3 -or $script:processCalls[0].arguments[0] -ne '/S' -or $script:processCalls[1].arguments[0] -ne '--version' -or -not $script:processCalls[2].observe -or $script:processCalls[2].timeout -ne 15) { throw 'Install/version/desktop sequence missing' }
    if ($FixtureDirectory) {
        Copy-Item $report (Join-Path $FixtureDirectory 'release.json')
        Copy-Item $setup $FixtureDirectory; Copy-Item $daemon $FixtureDirectory
    }
    Invoke-DefenderScan @($setup) $report 'release' 'test'
    if ($FixtureDirectory) { Copy-Item $report (Join-Path $FixtureDirectory 'singleton.json') }
    $script:extractFails=$true; Must-Fail { Invoke-DefenderScan @($daemon, $setup) $report 'release' 'test' }; $script:extractFails=$false
    foreach ($failure in @('/S', 'solud.exe', 'soluterminal-desktop.exe')) {
        $script:processFails=$failure; Must-Fail { Invoke-DefenderScan @($daemon, $setup) $report 'release' 'test' }
    }
    $script:processFails=''; $script:innerDetection=$true
    Must-Fail { Invoke-DefenderScan @($daemon, $setup) $report 'release' 'test' }; $script:innerDetection=$false
    $script:globalDetection=$true
    Must-Fail { Invoke-DefenderScan @($daemon, $setup) $report 'release' 'test' }
    $r=Get-Content -Raw $report | ConvertFrom-Json
    if ($r.status -ne 'detected' -or $r.unexpectedDetections.Count -ne 1) { throw 'Unrelated-path detection must fail release' }
    if ($FixtureDirectory) { Copy-Item $report (Join-Path $FixtureDirectory 'detected.json') }
    $defenderWsbCalls = New-Object 'System.Collections.Generic.List[object]'
    function Mock-Wsb { $defenderWsbCalls.Add(@($args)); $global:LASTEXITCODE=0 }
    function Get-Command { param([string]$Name) if ($Name -ne 'wsb.exe') { throw 'Unexpected command discovery' }; return @{Source='Mock-Wsb'} }
    $sandboxId='12345678-1234-1234-1234-123456789abc'
    & "$PSScriptRoot/defender-sandbox/host-lifecycle.ps1" -SandboxId $sandboxId -StopOnly
    if ($defenderWsbCalls.Count -ne 1 -or ($defenderWsbCalls[0] -join ' ') -ne "stop --id $sandboxId") { throw "Timeout recovery must stop only its own guest: count=$($defenderWsbCalls.Count), calls=$($defenderWsbCalls | ConvertTo-Json -Depth 3 -Compress)" }
    $defenderWsbCalls.Clear()
    $config=Join-Path $temp 'scan.wsb'; Set-Content $config '<Configuration/>'
    Must-Fail { & "$PSScriptRoot/defender-sandbox/host-lifecycle.ps1" -SandboxId $sandboxId -Config $config -Report (Join-Path $temp 'absent.json') -TimeoutSeconds -1 }
    if ($defenderWsbCalls.Count -ne 2 -or $defenderWsbCalls[1][0] -ne 'stop') { throw 'Lifecycle timeout must stop in finally' }
    Write-Host 'Defender Sandbox scanner and qualification offline tests passed'
} finally {
    $env:TEMP=$oldTemp; $env:COMPUTERNAME=$oldMachine
    Remove-Item -LiteralPath $temp -Recurse -Force
}
