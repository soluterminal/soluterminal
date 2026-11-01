#!/usr/bin/env python3
"""Stage exact candidate assets from WSL; scan only in Windows Sandbox."""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import uuid
from xml.sax.saxutils import escape

ROOT = Path(__file__).resolve().parent
FLAGGED_SHA = 'bc0c77082d57b2fe0adcdbb1b081d7d7f41d89e9c765abf0bd8101f821e07441'
POWERSHELL = '/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe'


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def winpath(path):
    return run('wslpath', '-w', str(path))


def asset_inputs(daemon, installer, version):
    if version == '0.0.1230':
        raise ValueError('Never stage 0.0.1230 on the Windows host; use --calibrate (guest-only download).')
    import re
    if not re.fullmatch(r'\d+\.\d+\.\d+', version or ''):
        raise ValueError('--version must be X.Y.Z')
    assets = []
    for source, name in ((daemon, 'solud-windows-amd64.exe'), (installer, f'soluterminal_{version}_x64-setup.exe')):
        path = Path(source).resolve(strict=True)
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        if digest == FLAGGED_SHA:
            raise ValueError('Refusing to copy the flagged 0.0.1230 binary to Windows disk.')
        assets.append((path, {'name': name, 'sha256': digest}))
    return assets


def configuration(input_path, output_path):
    return (ROOT / 'scan.wsb').read_text().replace('__INPUT__', escape(input_path)).replace('__OUTPUT__', escape(output_path))


def calibration_options():
    setting = os.environ.get('REQUIRE_CLOUD_CALIBRATION', '0')
    if setting not in ('0', '1'):
        raise ValueError('REQUIRE_CLOUD_CALIBRATION must be 0 or 1')
    required = setting == '1'
    url = os.environ.get('CLOUD_CALIBRATION_URL', '') if required else ''
    sha = os.environ.get('CLOUD_CALIBRATION_SHA256', '') if required else ''
    import re
    if required and (not url.startswith('https://') or not re.fullmatch('[a-f0-9]{64}', sha)):
        raise ValueError('Cloud calibration requires CLOUD_CALIBRATION_URL (HTTPS raw EXE) and CLOUD_CALIBRATION_SHA256')
    return dict(requireCloudCalibration=required, cloudCalibrationUrl=url, cloudCalibrationSha256=sha)


def launch(args):
    # Hash/reject before discovering or creating any Windows-side staging dir.
    assets = [] if args.calibrate else asset_inputs(args.daemon, args.installer, args.version)
    options = calibration_options() if args.calibrate else {}
    tools = []
    if not args.calibrate:
        seven_zip = Path(args.seven_zip)
        tools = [(seven_zip / name).resolve(strict=True) for name in ('7z.exe', '7z.dll')]
    report_dir = Path.home() / '.solu/defender-reports'
    report_dir.mkdir(parents=True, exist_ok=True)
    request_id = str(uuid.uuid4())
    windows_temp = run(POWERSHELL, '-NoProfile', '-NonInteractive', '-Command', '[Console]::Write($env:TEMP)')
    staging_parent = Path(run('wslpath', '-u', windows_temp))
    stage = Path(tempfile.mkdtemp(prefix='solu-defender-', dir=staging_parent))
    stopped = True
    try:
        input_dir = stage / 'input'
        output_dir = stage / 'output'
        input_dir.mkdir()
        output_dir.mkdir()
        for source, item in assets:
            shutil.copyfile(source, input_dir / item['name'])
        if tools:
            (input_dir / '7z').mkdir()
            for tool in tools:
                shutil.copyfile(tool, input_dir / '7z' / tool.name)
        shutil.copyfile(ROOT.parent / 'defender-scan.ps1', input_dir / 'defender-scan.ps1')
        shutil.copyfile(ROOT / 'sandbox-entry.ps1', input_dir / 'sandbox-entry.ps1')
        shutil.copyfile(ROOT / 'host-lifecycle.ps1', stage / 'host-lifecycle.ps1')
        kind = 'calibration' if args.calibrate else 'release'
        (input_dir / 'request.json').write_text(json.dumps({'kind': kind, 'requestId': request_id,
            'assets': [item for _, item in assets], **options}))
        config = stage / 'scan.wsb'
        config.write_text(configuration(winpath(input_dir), winpath(output_dir)), encoding='utf-8')
        lifecycle = [POWERSHELL, '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'RemoteSigned',
                     '-File', winpath(stage / 'host-lifecycle.ps1')]
        stopped = False
        try:
            result = subprocess.run([*lifecycle, '-Config', winpath(config),
                '-Report', winpath(output_dir / 'report.json'), '-SandboxId', request_id,
                '-TimeoutSeconds', str(args.timeout)], timeout=args.timeout + 120, capture_output=True,
                text=True, encoding='utf-8', errors='replace')
            stopped = result.returncode == 0
        except subprocess.TimeoutExpired:
            # subprocess.run killed the lifecycle before its finally could run.
            # Give stop its own process, without the scan timeout. Keep mapped
            # folders until it exits; retain them if closure is not confirmed.
            cleanup = subprocess.run([*lifecycle, '-SandboxId', request_id, '-StopOnly'],
                capture_output=True, text=True, encoding='utf-8', errors='replace')
            stopped = cleanup.returncode == 0
            if not stopped:
                raise ValueError(f'Sandbox timeout cleanup failed: {cleanup.stderr[-8192:]}')
            raise
        report_path = output_dir / 'report.json'
        if not report_path.is_file():
            raise ValueError('Sandbox produced no report; no clean result can be claimed.')
        report = json.loads(report_path.read_text(encoding='utf-8-sig'))
        if report.get('requestId') != request_id:
            raise ValueError('Sandbox report request ID mismatch.')
        destination = report_dir / f'{kind}-{request_id}.json'
        if result.returncode:
            report['error'] = 'Sandbox lifecycle failed; guest closure was not confirmed.'
            report['status'] = 'error'
            report['lifecycleDiagnostics'] = {'exitCode': result.returncode,
                'stdout': result.stdout[-8192:], 'stderr': result.stderr[-8192:]}
        destination.write_text(json.dumps(report, indent=2))
        print(json.dumps({'reportPath': str(destination), 'status': report.get('status'),
                          'assurance': report.get('assurance')}))
        if result.returncode:
            raise ValueError('Sandbox lifecycle failed; report retained for diagnosis.')
        if kind == 'calibration':
            sys.path.insert(0, str(ROOT.parent))
            from check_defender_report import calibrated
            if not calibrated(report):
                raise ValueError('CALIBRATION FAILED: MAPS, EICAR, pinned control, or required cloud evidence did not qualify. Inspect the JSON report.')
        else:
            sys.path.insert(0, str(ROOT.parent))
            from check_defender_report import valid_report
            expected = {item['name']: item['sha256'] for _, item in assets}
            if not valid_report(report, expected, dt.datetime.now(dt.timezone.utc)):
                raise ValueError('Defender did not qualify the exact candidate clean; inspect the JSON report.')

    finally:
        if stopped:
            shutil.rmtree(stage)
        else:
            print(f'Sandbox closure unconfirmed; retained mapped staging at {stage}', file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--daemon')
    parser.add_argument('--installer')
    parser.add_argument('--version')
    parser.add_argument('--seven-zip', default='/mnt/c/Program Files/7-Zip',
                        help='Installed Windows 7-Zip directory (7z.exe and 7z.dll), staged read-only for release scans')
    parser.add_argument('--calibrate', action='store_true')
    parser.add_argument('--timeout', type=int, default=1200)
    args = parser.parse_args()
    if not args.calibrate and not all((args.daemon, args.installer, args.version)):
        parser.error('supply --daemon, --installer and --version, or --calibrate')
    launch(args)


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        sys.exit(f'Defender Sandbox refused: {exc}')
