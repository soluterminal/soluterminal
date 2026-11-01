#!/usr/bin/env python3
"""Admit exact release hashes only from recent, active Windows Sandbox scans."""
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import sys

CALIBRATION_SHA = 'bc0c77082d57b2fe0adcdbb1b081d7d7f41d89e9c765abf0bd8101f821e07441'
CONTROL_SHA = 'c72004a764d17b383a4f2f4981c5608e7e628ccf2193e09162fc0056e79876dd'
REDUCED_ASSURANCE = 'reduced assurance: no flagged calibration sample available'


def load(path):
    return json.loads(Path(path).read_text(encoding='utf-8-sig'))


def protection_ok(p):
    return (isinstance(p, dict) and all(p.get(k) is True for k in (
        'serviceEnabled', 'antivirusEnabled', 'realTimeProtection', 'cloudProtection'))
        and p.get('runningMode') == 'Normal'
        and p.get('mapsReporting') == 2
        and all(isinstance(p.get(k), str) and p[k].strip() for k in (
            'engineVersion', 'platformVersion', 'signatureVersion')))


def common(report):
    return (isinstance(report, dict) and report.get('schemaVersion') in (1, 2) and report.get('sandbox') is True
            and bool(report.get('machineName')) and not report.get('error')
            and protection_ok(report.get('protection')) and protection_ok(report.get('protectionAfter')))


def sample_ok(sample, sha, verdict):
    if not isinstance(sample, dict) or sample.get('sha256') != sha or sample.get('verdict') != verdict:
        return False
    threats = sample.get('threats')
    if not isinstance(threats, list):
        return False
    if verdict == 'detected':
        return bool(threats) and all(isinstance(t, dict) and t.get('name') for t in threats)
    return type(sample.get('exitCode')) is int and sample['exitCode'] == 0 and not threats


def calibrated(report, require_cloud=None):
    if (not common(report) or report.get('schemaVersion') != 2
            or report.get('kind') != 'calibration' or report.get('status') != 'qualified'
            or report.get('assurance') != REDUCED_ASSURANCE):
        return False
    if require_cloud is None:
        setting = os.environ.get('REQUIRE_CLOUD_CALIBRATION', '0')
        if setting not in ('0', '1'):
            return False
        require_cloud = setting == '1'
    q = report.get('qualification')
    if not isinstance(q, dict) or not isinstance(q.get('maps'), dict):
        return False
    if type(q['maps'].get('exitCode')) is not int or q['maps']['exitCode'] != 0:
        return False
    eicar = q.get('eicarThreats')
    if not isinstance(eicar, list) or not any(
            isinstance(t, dict) and isinstance(t.get('name'), str) and 'eicar' in t['name'].lower() for t in eicar):
        return False
    historical = q.get('historical')
    if (not isinstance(historical, dict) or historical.get('informational') is not True
            or not any(sample_ok(historical, CALIBRATION_SHA, v) for v in ('clean', 'detected'))
            or not sample_ok(q.get('control'), CONTROL_SHA, 'clean')):
        return False
    cloud = q.get('cloud')
    if not isinstance(cloud, dict) or type(cloud.get('required')) is not bool or type(cloud.get('calibrated')) is not bool:
        return False
    if require_cloud and not cloud['required']:
        return False
    if cloud['required'] or cloud['calibrated']:
        off, on = cloud.get('off'), cloud.get('on')
        sha = off.get('sha256') if isinstance(off, dict) else None
        return (isinstance(sha, str) and re.fullmatch('[a-f0-9]{64}', sha) is not None
                and cloud.get('scanOnly') is True
                and off.get('execution') is None and isinstance(on, dict) and on.get('execution') is None
                and cloud['calibrated'] is True and sample_ok(off, sha, 'clean') and sample_ok(on, sha, 'detected'))
    return cloud.get('off') is None and cloud.get('on') is None


def parse_timestamp(value):
    # .NET's round-trip format has seven fractional digits; Python <=3.10
    # accepts at most six. Preserve microsecond precision on every host.
    value = re.sub(r'(\.\d{6})\d+(?=[+-]\d{2}:\d{2}$)', r'\1', value.replace('Z', '+00:00'))
    return dt.datetime.fromisoformat(value)


def clean_file(entry):
    return (isinstance(entry, dict) and entry.get('verdict') == 'clean'
            and type(entry.get('exitCode')) is int and entry['exitCode'] == 0
            and entry.get('threats') == []
            and isinstance(entry.get('sha256'), str)
            and re.fullmatch('[a-f0-9]{64}', entry['sha256']) is not None)


def installer_ok(report, expected):
    installers = report.get('installers')
    if not isinstance(installers, list) or len(installers) != 1:
        return False
    i = installers[0]
    if (not isinstance(i, dict) or i.get('status') != 'clean' or i.get('error')
            or i.get('name') not in expected or not i['name'].endswith('-setup.exe')
            or i.get('sha256') != expected[i['name']]):
        return False
    contents = i.get('contents')
    if not isinstance(contents, list) or len(contents) < 2 or not all(clean_file(e) for e in contents):
        return False
    for key in ('install', 'daemon'):
        e = i.get(key)
        if not isinstance(e, dict) or type(e.get('exitCode')) is not int or e['exitCode'] != 0:
            return False
    desktop = i.get('desktop')
    return (isinstance(desktop, dict) and desktop.get('started') is True
            and type(desktop.get('observedSeconds')) is int and desktop['observedSeconds'] >= 15
            and (desktop.get('exitCode') is None or
                 (type(desktop.get('exitCode')) is int and desktop['exitCode'] == 0)))


def calibration_before(report, scan):
    try:
        start = parse_timestamp(report['startedAt'])
        finish = parse_timestamp(report['finishedAt'])
        scan_start = parse_timestamp(scan['startedAt'])
        return scan_start - dt.timedelta(days=7) <= finish < scan_start and start <= finish
    except (KeyError, ValueError, TypeError, AttributeError):
        return False


def valid_report(report, expected, now):
    if not common(report) or report.get('schemaVersion') != 2 or report.get('kind') != 'release' or report.get('status') != 'clean':
        return False
    try:
        start = parse_timestamp(report['startedAt'])
        finish = parse_timestamp(report['finishedAt'])
        if not (now - dt.timedelta(hours=24) <= start <= finish <= now):
            return False
        files = report['files']
        actual = {f['name']: f['sha256'] for f in files}
        return (len(files) == len(expected) and actual == expected
                and all(clean_file(f) for f in files)
                and report.get('unexpectedDetections') == [] and installer_ok(report, expected))
    except (KeyError, ValueError, TypeError, AttributeError):
        return False


def require_report(directory, assets, version, now=None):
    now = now or dt.datetime.now(dt.timezone.utc)
    expected = {name: hashlib.sha256((Path(assets) / name).read_bytes()).hexdigest()
                for name in ('solud-windows-amd64.exe', f'soluterminal_{version}_x64-setup.exe')}
    reports = []
    for path in sorted(Path(directory).glob('*.json')):
        try:
            report = load(path)
            if isinstance(report, dict):
                reports.append((path, report))
        except (OSError, ValueError):
            continue
    calibrations = [r for _, r in reports if calibrated(r)]
    if not calibrations:
        raise ValueError('no successful Sandbox qualification (MAPS, EICAR, pinned control and required cloud calibration)')
    for path, report in reports:
        if valid_report(report, expected, now) and any(calibration_before(c, report) for c in calibrations):
            p = report['protection']
            print(f'Defender clean: {path}; machine {report["machineName"]}; '
                  f'engine {p["engineVersion"]}; signature {p["signatureVersion"]}; {report["finishedAt"]}; {REDUCED_ASSURANCE}')
            return path
    raise ValueError('no clean Windows Sandbox report under 24 h old matches both exact release asset hashes with qualification finished within 7 days before its start')


if __name__ == '__main__':
    try:
        require_report(*sys.argv[1:])
    except (OSError, ValueError, KeyError, TypeError) as exc:
        sys.exit(f'Defender report refused: {exc}')
