import copy
import datetime as dt
import hashlib
import json
from pathlib import Path
from unittest.mock import patch
import tempfile
import unittest
from check_defender_report import CALIBRATION_SHA, CONTROL_SHA, REDUCED_ASSURANCE, calibrated, parse_timestamp, require_report, valid_report


def protection():
    return dict(serviceEnabled=True, antivirusEnabled=True, realTimeProtection=True,
        cloudProtection=True, mapsReporting=2, runningMode='Normal', engineVersion='1.2.3',
        platformVersion='4.5.6', signatureVersion='7.8.9')


def fixture_reports(assets, version, now=None):
    now = now or dt.datetime.now(dt.timezone.utc)
    common = dict(schemaVersion=2, sandbox=True, machineName='SandboxFixture',
        startedAt=(now-dt.timedelta(minutes=2)).isoformat(),
        finishedAt=(now-dt.timedelta(minutes=1)).isoformat(), error=None,
        protection=protection(), protectionAfter=protection())
    release = dict(common, kind='release', status='clean', files=[
        dict(name=name, sha256=hashlib.sha256((Path(assets)/name).read_bytes()).hexdigest(),
             verdict='clean', exitCode=0, threats=[])
        for name in ('solud-windows-amd64.exe', f'soluterminal_{version}_x64-setup.exe')])
    release['unexpectedDetections'] = []
    release['installers'] = [dict(name=release['files'][1]['name'], sha256=release['files'][1]['sha256'],
        status='clean', error=None, contents=[dict(name=name, sha256='a'*64, verdict='clean', exitCode=0, threats=[])
            for name in ('solud.exe', 'soluterminal-desktop.exe')],
        install=dict(exitCode=0), daemon=dict(exitCode=0, output='solud 1.2.3'),
        desktop=dict(started=True, observedSeconds=15, exitCode=None))]
    calibration = dict(common, schemaVersion=2, kind='calibration', status='qualified',
        startedAt=(now-dt.timedelta(minutes=4)).isoformat(),
        finishedAt=(now-dt.timedelta(minutes=3)).isoformat(),
        assurance=REDUCED_ASSURANCE, qualification=dict(
            maps=dict(exitCode=0), eicarThreats=[dict(name='Virus:DOS/EICAR_Test_File')],
            historical=dict(sha256=CALIBRATION_SHA, verdict='clean', exitCode=0, threats=[], informational=True),
            control=dict(sha256=CONTROL_SHA, verdict='clean', exitCode=0, threats=[]),
            cloud=dict(required=False, calibrated=False, off=None, on=None)))
    return release, calibration


class ReportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.assets = self.root/'assets'; self.assets.mkdir()
        self.reports = self.root/'reports'; self.reports.mkdir()
        for name in ('solud-windows-amd64.exe', 'soluterminal_1.2.3_x64-setup.exe'):
            (self.assets/name).write_bytes(name.encode())
        self.now = dt.datetime.now(dt.timezone.utc)
        self.report, self.calibration = fixture_reports(self.assets, '1.2.3', self.now)
        self.expected = {f['name']: f['sha256'] for f in self.report['files']}

    def write(self):
        (self.reports/'scan.json').write_text(json.dumps(self.report))
        (self.reports/'calibration.json').write_text(json.dumps(self.calibration))

    def test_exact_clean_assets_and_calibration_required(self):
        self.write()
        require_report(self.reports, self.assets, '1.2.3', self.now)
        (self.reports/'calibration.json').unlink()
        with self.assertRaises(ValueError):
            require_report(self.reports, self.assets, '1.2.3', self.now)

    def test_wrong_stale_future_missing_or_detected_results_fail(self):
        changes = [
            lambda r: r['files'][0].update(sha256='0'*64),
            lambda r: r['files'].pop(),
            lambda r: r['files'].append(r['files'][0]),
            lambda r: r['files'][0].update(verdict='detected'),
            lambda r: r['files'][0].update(exitCode=2),
            lambda r: r['files'][0].update(exitCode=False),
            lambda r: r['files'][0].update(exitCode=0.0),
            lambda r: r.update(startedAt=False),
            lambda r: r.update(files=[None]),
            lambda r: r['files'][0].update(threats=[{'name': 'any'}]),
            lambda r: r.update(startedAt=(self.now-dt.timedelta(hours=25)).isoformat()),
            lambda r: r.update(finishedAt=(self.now+dt.timedelta(minutes=1)).isoformat()),
            lambda r: r.update(startedAt='bad'),
            lambda r: r.update(sandbox=False),
            lambda r: r.update(error='lifecycle failed'),
            lambda r: r['protection'].update(cloudProtection=False),
            lambda r: r['protectionAfter'].update(realTimeProtection=False),
            lambda r: r['protection'].update(platformVersion=''),
        ]
        for change in changes:
            r = copy.deepcopy(self.report); change(r)
            with self.subTest(report=r):
                self.assertFalse(valid_report(r, self.expected, self.now))

    def test_powershell_seven_fractional_digit_timestamps(self):
        self.assertEqual(parse_timestamp('2026-11-01T00:00:00.1234567Z').microsecond, 123456)
        for key in ('startedAt', 'finishedAt'):
            self.report[key] = self.report[key].replace('+00:00', '7Z')
        self.assertTrue(valid_report(self.report, self.expected, self.now))

    def test_qualification_accepts_clean_or_detected_historical_sample(self):
        self.assertTrue(calibrated(self.calibration))
        self.calibration['qualification']['historical'].update(verdict='detected', exitCode=2, threats=[dict(name='Bearfoos')])
        self.assertTrue(calibrated(self.calibration))

    def test_qualification_fails_closed_on_missing_or_bad_evidence(self):
        changes = [
            lambda r: r.update(schemaVersion=1),
            lambda r: r.update(status='error'),
            lambda r: r.update(assurance='full'),
            lambda r: r.update(error='download failed'),
            lambda r: r['protectionAfter'].update(mapsReporting=0),
            lambda r: r['qualification']['maps'].update(exitCode=2),
            lambda r: r['qualification']['maps'].update(exitCode=False),
            lambda r: r['qualification'].update(eicarThreats=[]),
            lambda r: r['qualification'].update(eicarThreats=[{'name':'Other'}]),
            lambda r: r['qualification']['control'].update(sha256='0'*64),
            lambda r: r['qualification']['control'].update(verdict='detected'),
            lambda r: r['qualification']['control'].update(exitCode=2),
            lambda r: r['qualification']['control'].update(threats=[{'name':'Bearfoos'}]),
            lambda r: r['qualification']['historical'].update(sha256='0'*64),
            lambda r: r['qualification']['historical'].update(verdict='error'),
            lambda r: r['qualification']['historical'].update(informational=False),
            lambda r: r['qualification']['cloud'].update(required=True),
        ]
        for change in changes:
            r = copy.deepcopy(self.calibration); change(r)
            with self.subTest(report=r):
                self.assertFalse(calibrated(r))
        for key in ('maps', 'eicarThreats', 'historical', 'control', 'cloud'):
            for value in (None, [], 'bad'):
                r = copy.deepcopy(self.calibration); r['qualification'][key] = value
                self.assertFalse(calibrated(r))

    def test_required_cloud_must_prove_same_hash_off_clean_on_detected(self):
        self.assertFalse(calibrated(self.calibration, require_cloud=True))
        with patch.dict('os.environ', {'REQUIRE_CLOUD_CALIBRATION': '1'}):
            self.assertFalse(calibrated(self.calibration))
        q = self.calibration['qualification']
        q['cloud'] = dict(required=True, calibrated=True, scanOnly=True,
            off=dict(sha256='a'*64, verdict='clean', exitCode=0, threats=[]),
            on=dict(sha256='a'*64, verdict='detected', exitCode=2, threats=[dict(name='cloud sample')]))
        self.assertTrue(calibrated(self.calibration, require_cloud=True))
        for leg, fields in [('off', dict(verdict='detected', threats=[dict(name='offline')])),
                            ('on', dict(verdict='clean', exitCode=0, threats=[])),
                            ('on', dict(sha256='b'*64)), ('off', dict(execution={'exitCode': 0}))]:
            r = copy.deepcopy(self.calibration); r['qualification']['cloud'][leg].update(fields)
            self.assertFalse(calibrated(r, require_cloud=True))

    def test_calibration_freshness_and_ordering(self):
        scan_start = parse_timestamp(self.report['startedAt'])
        for offset, accepted in [(dt.timedelta(days=7), True),
                                 (dt.timedelta(days=7, microseconds=1), False),
                                 (dt.timedelta(microseconds=1), True),
                                 (dt.timedelta(0), False), (dt.timedelta(seconds=-1), False)]:
            finish = scan_start - offset
            self.calibration.update(startedAt=(finish-dt.timedelta(minutes=1)).isoformat(), finishedAt=finish.isoformat())
            self.write()
            with self.subTest(offset=offset):
                if accepted:
                    require_report(self.reports, self.assets, '1.2.3', self.now)
                else:
                    with self.assertRaisesRegex(ValueError, '7 days'):
                        require_report(self.reports, self.assets, '1.2.3', self.now)
        for bad in (None, False, 'bad', '2026-11-01T00:00:00'):
            self.calibration['finishedAt'] = bad
            self.write()
            with self.assertRaises(ValueError):
                require_report(self.reports, self.assets, '1.2.3', self.now)

    def test_installer_coverage_and_global_detections_required(self):
        changes = [
            lambda r: r.update(schemaVersion=1),
            lambda r: r.pop('installers'),
            lambda r: r.update(installers=[]),
            lambda r: r['installers'][0].update(sha256='b'*64),
            lambda r: r['installers'][0].update(status='error'),
            lambda r: r['installers'][0].update(contents=[]),
            lambda r: r['installers'][0]['contents'][0].update(exitCode=False),
            lambda r: r['installers'][0]['contents'][0].update(verdict='detected'),
            lambda r: r['installers'][0]['install'].update(exitCode=2),
            lambda r: r['installers'][0]['daemon'].update(exitCode=0.0),
            lambda r: r['installers'][0]['desktop'].update(started=False),
            lambda r: r['installers'][0]['desktop'].update(observedSeconds=14),
            lambda r: r['installers'][0]['desktop'].update(exitCode=1),
            lambda r: r.pop('unexpectedDetections'),
            lambda r: r.update(unexpectedDetections=[dict(name='unrelated path')]),
        ]
        for change in changes:
            report = copy.deepcopy(self.report); change(report)
            with self.subTest(report=report):
                self.assertFalse(valid_report(report, self.expected, self.now))

    def test_asset_mutation_refused(self):
        self.write()
        (self.assets/'solud-windows-amd64.exe').write_bytes(b'changed')
        with self.assertRaises(ValueError):
            require_report(self.reports, self.assets, '1.2.3', self.now)


if __name__ == '__main__':
    unittest.main()
