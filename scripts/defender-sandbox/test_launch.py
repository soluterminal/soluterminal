import importlib.util
import contextlib
import io
import json
import subprocess
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import xml.etree.ElementTree as ET

spec = importlib.util.spec_from_file_location('sandbox_launch', Path(__file__).with_name('launch.py'))
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class LauncherTests(unittest.TestCase):
    def test_guest_request_and_launcher_output_contain_only_data(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); staging = root/'staging'; staging.mkdir()
            home = root/'home'; home.mkdir()
            requests = []
            return_code = 0

            def host_lifecycle(command, **kwargs):
                stage = next(staging.iterdir())
                request = json.loads((stage/'input/request.json').read_text())
                requests.append(request)
                self.assertEqual({p.name for p in (stage/'input').iterdir()},
                    {'request.json', 'defender-scan.ps1', 'sandbox-entry.ps1'})
                (stage/'output/report.json').write_text(json.dumps(dict(
                    requestId=request['requestId'], status='qualified', assurance='reduced assurance: no flagged calibration sample available')))
                self.assertTrue(kwargs['capture_output'])
                return SimpleNamespace(returncode=return_code, stdout='mock start', stderr='mock stop failure')

            for return_code in (0, 1):
                output = io.StringIO()
                with patch.dict(launcher.os.environ, {}, clear=True), patch.object(launcher.Path, 'home', return_value=home), \
                        patch.object(launcher, 'run', side_effect=lambda *a: str(staging)), \
                        patch.object(launcher, 'winpath', side_effect=str), \
                        patch.object(launcher.subprocess, 'run', side_effect=host_lifecycle), \
                        patch.dict('sys.modules', {'check_defender_report': SimpleNamespace(calibrated=lambda r: True)}), \
                        contextlib.redirect_stdout(output):
                    if return_code:
                        with self.assertRaisesRegex(ValueError, 'lifecycle failed'):
                            launcher.launch(SimpleNamespace(calibrate=True, timeout=1))
                    else:
                        launcher.launch(SimpleNamespace(calibrate=True, timeout=1))
                evidence = json.loads(output.getvalue())
                self.assertEqual(evidence['status'], 'error' if return_code else 'qualified')
                saved = json.loads(Path(evidence['reportPath']).read_text())
                if return_code:
                    self.assertEqual(saved['lifecycleDiagnostics']['stderr'], 'mock stop failure')
            self.assertFalse(requests[0]['requireCloudCalibration'])
            self.assertEqual(requests[0]['assets'], [])

    def test_timeout_stop_finishes_before_mapped_folders_removed(self):
        for cleanup_code in (0, 1):
            with self.subTest(cleanup_code=cleanup_code), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp); staging = root/'staging'; staging.mkdir()
                home = root/'home'; home.mkdir()
                calls = []
                def lifecycle(command, **kwargs):
                    calls.append(command)
                    stage = next(staging.iterdir())
                    self.assertTrue((stage/'input/request.json').is_file())
                    if '-StopOnly' not in command:
                        self.assertEqual(kwargs['timeout'], 121)
                        raise subprocess.TimeoutExpired(command, 121)
                    self.assertNotIn('timeout', kwargs, 'cleanup must not inherit the scan timeout')
                    self.assertEqual(command[command.index('-SandboxId')+1],
                                     calls[0][calls[0].index('-SandboxId')+1])
                    return SimpleNamespace(returncode=cleanup_code, stderr='mock stop failure')
                with patch.dict(launcher.os.environ, {}, clear=True), patch.object(launcher.Path, 'home', return_value=home), \
                        patch.object(launcher, 'run', return_value=str(staging)), \
                        patch.object(launcher, 'winpath', side_effect=str), \
                        patch.object(launcher.subprocess, 'run', side_effect=lifecycle), \
                        contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(ValueError if cleanup_code else subprocess.TimeoutExpired):
                        launcher.launch(SimpleNamespace(calibrate=True, timeout=1))
                self.assertEqual(len(calls), 2)
                self.assertEqual(bool(list(staging.iterdir())), bool(cleanup_code))

    def test_release_stages_seven_zip_read_only_with_exact_assets(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); staging = root/'staging'; staging.mkdir()
            home = root/'home'; home.mkdir()
            tool = root/'7zip'; tool.mkdir()
            for name in ('7z.exe', '7z.dll'):
                (tool/name).write_bytes(name.encode())
            daemon = root/'daemon'; daemon.write_bytes(b'fixture daemon')
            installer = root/'installer'; installer.write_bytes(b'fixture installer')
            def lifecycle(command, **kwargs):
                stage = next(staging.iterdir()); inputs = stage/'input'
                for name in ('7z.exe', '7z.dll'):
                    self.assertEqual((inputs/'7z'/name).read_bytes(), name.encode())
                request = json.loads((inputs/'request.json').read_text())
                (stage/'output/report.json').write_text(json.dumps(dict(requestId=request['requestId'], status='clean')))
                return SimpleNamespace(returncode=0)
            with patch.object(launcher.Path, 'home', return_value=home), \
                    patch.object(launcher, 'run', return_value=str(staging)), \
                    patch.object(launcher, 'winpath', side_effect=str), \
                    patch.object(launcher.subprocess, 'run', side_effect=lifecycle), \
                    patch.dict('sys.modules', {'check_defender_report': SimpleNamespace(valid_report=lambda *a: True)}), \
                    contextlib.redirect_stdout(io.StringIO()):
                launcher.launch(SimpleNamespace(calibrate=False, daemon=daemon, installer=installer,
                    version='1.2.3', timeout=1, seven_zip=tool))
            self.assertFalse(list(staging.iterdir()))

    def test_cloud_calibration_is_explicit_and_pinned(self):
        with patch.dict(launcher.os.environ, {}, clear=True):
            self.assertFalse(launcher.calibration_options()['requireCloudCalibration'])
            with patch.dict(launcher.os.environ, {'REQUIRE_CLOUD_CALIBRATION':'1'}):
                with self.assertRaises(ValueError):
                    launcher.calibration_options()
            with patch.dict(launcher.os.environ, {'REQUIRE_CLOUD_CALIBRATION':'invalid'}):
                with self.assertRaises(ValueError):
                    launcher.calibration_options()
            with patch.dict(launcher.os.environ, {'REQUIRE_CLOUD_CALIBRATION':'1',
                    'CLOUD_CALIBRATION_URL':'https://example.test/cloud.exe', 'CLOUD_CALIBRATION_SHA256':'a'*64}):
                self.assertEqual(launcher.calibration_options(), dict(requireCloudCalibration=True,
                    cloudCalibrationUrl='https://example.test/cloud.exe', cloudCalibrationSha256='a'*64))

    def test_read_only_input_and_only_output_writable(self):
        xml = ET.fromstring(launcher.configuration('C:\\Temp\\a&b', 'C:\\Temp\\out'))
        mappings = xml.findall('MappedFolders/MappedFolder')
        self.assertEqual([m.findtext('ReadOnly') for m in mappings], ['true', 'false'])
        self.assertEqual(mappings[0].findtext('HostFolder'), 'C:\\Temp\\a&b')
        self.assertEqual(xml.findtext('ClipboardRedirection'), 'Disable')
        self.assertIn('sandbox-entry.ps1', xml.findtext('LogonCommand/Command'))

    def test_1230_and_flagged_hash_rejected_before_windows_staging(self):
        with self.assertRaises(ValueError):
            launcher.asset_inputs('missing', 'missing', '0.0.1230')
        with tempfile.TemporaryDirectory() as tmp:
            a = Path(tmp)/'a'; a.write_bytes(b'fixture')
            with patch.object(launcher, 'FLAGGED_SHA', launcher.hashlib.sha256(b'fixture').hexdigest()):
                with self.assertRaises(ValueError):
                    launcher.asset_inputs(a, a, '1.2.3')


if __name__ == '__main__':
    unittest.main()
