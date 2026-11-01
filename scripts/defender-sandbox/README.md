# Windows Sandbox Defender qualification

Host Defender scans are never used. The host launcher only stages input, starts
its own disposable Sandbox, waits for JSON, and stops that exact Sandbox ID.
The input mapping is read-only; the output mapping carries JSON only. Files are
scanned from guest-local scratch. No exclusions are added and real-time protection
is never disabled. Only explicitly required cloud calibration may temporarily
turn MAPS off inside the guest, restoring Advanced in a `finally` block.
GitHub's passive hosted-runner Defender result is informational only; the active
hosted-runner probe on `ci/defender-runner-probe` is the qualification reference.
Sandbox cloud protection is on, so Microsoft may receive candidate binaries
before release; this is acceptable because the source is public.

## Setup and weekly calibration

The maintainer enables **Windows Sandbox** in Windows Features once on Windows 11 Pro,
then restarts if Windows asks. The launcher requires the Windows 11 24H2 `wsb.exe`
CLI (with `start --id` and `stop --id`) for reliable ownership and shutdown. It
fails if this CLI is unavailable; it never kills another Sandbox session.

Before trusting this gate, run from WSL:

```sh
python3 scripts/defender-sandbox/launch.py --calibrate
```

Microsoft has cleared the former **0.0.1230** Bearfoos false positive. Calibration
now uses the same reduced-assurance qualification as the active hosted probe:

1. Updated signatures, active real-time protection, Normal mode, MAPS Advanced,
   cloud sample submission and block-at-first-seen enabled, and no exclusions.
2. `MpCmdRun -ValidateMapsConnection` exits zero.
3. EICAR is detected on access (no on-demand EICAR scan).
4. **0.0.1230** is scanned, marked with a Mark of the Web and, if clean, run with
   `--version`. Its verdict is **informational**: clean or detected both qualify.
5. The pinned **0.0.1227** control must scan clean:
   `c72004a764d17b383a4f2f4981c5608e7e628ccf2193e09162fc0056e79876dd`.
6. Full protection is verified again at the end.

The published **0.0.1230** daemon is downloaded **only inside the guest**, hashed
in guest memory against:
`bc0c77082d57b2fe0adcdbb1b081d7d7f41d89e9c765abf0bd8101f821e07441`.
It is never copied into the host input/output folder or the maintainer's Windows disk.
Only the JSON calibration report leaves the guest. Version 2 calibration reports
have `status: qualified`, individual `qualification` results, and
`assurance: reduced assurance: no flagged calibration sample available`.
This establishes connectivity and local detection, **not cloud-dependent malware
detection**. Old Bearfoos-only reports are no longer accepted. Protection off,
failed signature updates, downloads or hashes, absent Defender, missing EICAR
evidence, a dirty control, and scan errors fail closed. Do not enable protection
by script, add exclusions, or substitute a passive VM. No successful real
Sandbox run is claimed by this code change.

### Optional cloud-dependent calibration

`REQUIRE_CLOUD_CALIBRATION=1` requires a pinned raw executable supplied through
`CLOUD_CALIBRATION_URL` (HTTPS) and `CLOUD_CALIBRATION_SHA256`. These options are
copied as data into the guest request; the host never downloads the sample.
For example, once a suitable sample is available:

```sh
REQUIRE_CLOUD_CALIBRATION=1 CLOUD_CALIBRATION_URL=https://example.org/sample.exe \
  CLOUD_CALIBRATION_SHA256=REPLACE_WITH_64_LOWERCASE_HEX_DIGITS \
  python3 scripts/defender-sandbox/launch.py --calibrate
```

The guest tests MAPS **off first**, then Advanced, using the same pinned bytes
on distinct guest paths, adding a Mark of the Web and scanning on demand.
Third-party cloud samples are never executed. Cloud evidence must record
`scanOnly: true`; reports from the former execution-based legs are refused. Only clean off / detected on qualifies. MAPS
Advanced must be restored even if the off leg fails; real-time protection stays
on throughout. The default is off: no MAPS changes or cloud sample downloads.
Setting `REQUIRE_CLOUD_CALIBRATION=1` on `publish-official.sh` also requires this
evidence, preventing a default qualification from satisfying that request.

There is no bundled qualifying cloud sample today. Microsoft's demo
(`https://go.microsoft.com/fwlink/?linkid=2298135`, ZIP password `infected`,
requires 7-Zip) and AMTSO CloudCar both detect offline and cannot prove cloud
dependence. Unlike the hosted experiment, the Sandbox gate never disables
real-time protection to extract a sample. Missing sample configuration fails
closed when cloud calibration is required.

A qualified calibration must finish strictly before the release scan starts,
and no more than seven days before it. Its start must precede its finish.
The last successful qualification date is `finishedAt` in the saved calibration
report; publish output shows the candidate scan date and reduced assurance.

## Scan an exact candidate

Install Windows 7-Zip first. The launcher stages `7z.exe` and `7z.dll` from
`/mnt/c/Program Files/7-Zip` into the read-only input mapping; use
`--seven-zip /path/to/7-Zip` for a different installation. Missing tools refuse
before Windows staging. Calibration does not need 7-Zip.

After the official-build verifier supplies the exact release assets:

```sh
python3 scripts/defender-sandbox/launch.py --version X.Y.Z \
  --daemon /path/to/verified/solud-windows-amd64.exe \
  --installer /path/to/verified/soluterminal_X.Y.Z_x64-setup.exe
```

The launcher rejects the known flagged hash and version 0.0.1230 **before** any
host staging. Calibration is the only supported way to fetch that specimen.
The guest updates Defender signatures, requires active real-time and cloud
protection, and scans with `MpCmdRun -DisableRemediation` inside the guest only.
Its JSON records each actual SHA-256 and verdict, threat names, scanner exits,
engine/platform/signature versions, cloud-protection flag, machine name and UTC
timestamps. Release report schema version 2 also requires:

- 7-Zip NSIS extraction and a separate scan/hash for every extracted EXE
  (at least two executables must be found).
- Silent `/S` installation, installed `solud.exe --version` with exit zero,
  and a 15-second launch observation of `soluterminal-desktop.exe`.
- No Defender detections anywhere since scan entry, including installed files
  and child processes. Qualification uses a separate guest, so no qualification
  paths are exempted in release scans.

An early desktop exit with a nonzero code fails; a clean early exit (for example,
a launcher handing off) is recorded. Process timeouts, extraction/install/launch
errors, missing binaries, any detection or final protection failure refuse.
Version 1 release reports lack this coverage and are no longer accepted.
Only JSON is returned to `~/.solu/defender-reports/`; the guest is
stopped even after errors or timeout. On a Python timeout, a separate lifecycle
`-StopOnly` process runs without the scan timeout before staging can be deleted.
If closure is unconfirmed, staging is retained and its path reported on stderr.
The scan timeout defaults to 20 minutes.

`publish-official.sh` requires a successful version 2 qualification and one clean
version 2 Sandbox report whose two exact names/hashes match the staged `$DIST`
daemon and installer that will be signed and uploaded. Both protection snapshots must be active; the scan must have started
and finished within the past 24 hours. Report changes or a new asset hash require
a new scan. `--skip-defender-check=REASON` is the only explicit override; other
release approval and provenance gates remain in force.

Treat reports as local operator evidence, protected like other release records;
they are not signed attestations. A writable local JSON file cannot protect
against a hostile account that can forge it. Recalibrate if the scan environment
changes or EICAR stops reproducing. Python launcher stdout is JSON only; failures
also have a diagnostic on stderr.

## Offline verification

```sh
python3 -m unittest discover -s scripts -p test_check_defender_report.py -v
python3 -m unittest discover -s scripts/defender-sandbox -p test_launch.py -v
python3 -m unittest discover -s scripts -p test_defender_powershell_fixture.py -v
bash scripts/test-publish-official.sh
```

PowerShell tests mock Defender, downloads, protection changes and sample execution;
they never scan or change Defender on the host.

The fixture test runs the mocked scanner, calibration and installer paths in real
Windows PowerShell 5.1, then admits/refuses their serialized JSON with Python.
It covers empty and singleton arrays, seven-digit timestamps and the real process
helper using inert OS commands; it skips locally without Windows PowerShell.
The public official build requires it and fails if PowerShell is unavailable.

Four-part team build scratch sources older than 24 hours are swept at the next
build within that caller's scratch directory, matching only directories named
`windows-source.??????`. Fresh sources, symlinks and broader matches are kept;
preparation failures remove their newly created source immediately.
