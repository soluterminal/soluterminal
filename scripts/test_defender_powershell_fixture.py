"""Run inert mocked scanner paths in real Windows PowerShell 5.1, validate its JSON."""
import datetime as dt
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from check_defender_report import calibrated, load, require_report, valid_report

POWERSHELL = (shutil.which('powershell.exe') if os.name == 'nt' else
              '/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe')


POWERSHELL_AVAILABLE = bool(POWERSHELL and Path(POWERSHELL).is_file() and
                            (os.name == 'nt' or shutil.which('wslpath')))


@unittest.skipUnless(POWERSHELL_AVAILABLE, 'requires Windows PowerShell 5.1')
class PowerShellFixtureTests(unittest.TestCase):
    def test_real_powershell_serialization_admitted_by_python(self):
        with tempfile.TemporaryDirectory() as tmp:
            script = Path(__file__).with_name('test-defender-scan.ps1').resolve()
            def winpath(path):
                if os.name == 'nt':
                    return str(path)
                return subprocess.check_output(['wslpath', '-w', str(path)], text=True).strip()
            result = subprocess.run([POWERSHELL, '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', winpath(script),
                                     '-FixtureDirectory', winpath(tmp)], capture_output=True, text=True, timeout=120)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            root = Path(tmp)
            calibration, cloud = load(root/'calibration.json'), load(root/'cloud.json')
            self.assertTrue(calibrated(calibration))
            self.assertTrue(calibrated(cloud, require_cloud=True))
            self.assertIsInstance(calibration['qualification']['eicarThreats'], list)
            self.assertEqual(len(calibration['qualification']['eicarThreats']), 1)
            self.assertEqual(cloud['qualification']['cloud']['off']['threats'], [])
            self.assertEqual(len(cloud['qualification']['cloud']['on']['threats']), 1)
            now = dt.datetime.now(dt.timezone.utc)
            for name, accepted in [('release', True), ('singleton', True), ('detected', False)]:
                report = load(root/(name+'.json'))
                expected = {f['name']: f['sha256'] for f in report['files']}
                self.assertEqual(valid_report(report, expected, now), accepted, name)
                self.assertEqual(report['files'][0]['threats'], [])
                self.assertIsInstance(report['installers'], list)
            self.assertEqual(len(load(root/'singleton.json')['files']), 1)
            # End-to-end publication checker hashes the inert fixture assets too.
            for name in ('singleton.json', 'detected.json'):
                (root/name).unlink()
            require_report(root, root, '1.2.3', now)


if __name__ == '__main__':
    if os.environ.get('REQUIRE_POWERSHELL_FIXTURE') == '1' and not POWERSHELL_AVAILABLE:
        raise SystemExit('Windows PowerShell 5.1 fixture is required; refusing a skipped build test')
    unittest.main()
