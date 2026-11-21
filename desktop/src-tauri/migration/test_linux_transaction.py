"""Only explicit synthetic temp directories. No HOME/profile or daemon use."""
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import pwd
import select
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

SOURCE = Path(__file__).with_name("linux_transaction.py")
SPEC = importlib.util.spec_from_file_location("transaction_fixture_resource", SOURCE)
TX = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TX)
NONCE = "a" * 32

# Unit-only dependency injection; no production flag/env bypass exists. File
# operations still execute on real owned temp dirs on tmpfs/xfs/overlay/etc.
CHILD_UNIT = """import importlib.util,sys
source=sys.argv[1]
spec=importlib.util.spec_from_file_location('transaction_child_fixture',source)
module=importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.native_filesystem=lambda fd: None
sys.argv=sys.argv[1:]
sys.exit(module.main())
"""


def private(path, value):
    data = value if isinstance(value, bytes) else json.dumps(value).encode()
    fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())


class Transaction(unittest.TestCase):
    def setUp(self):
        try:
            self.user_name = pwd.getpwuid(os.geteuid()).pw_name
        except KeyError:
            self.skipTest("fixture requires a mapped Linux service user")
        policy = mock.patch.object(TX, "native_filesystem", return_value=None)
        policy.start()
        self.addCleanup(policy.stop)
        self.scratch = tempfile.TemporaryDirectory(prefix="solu-migration-transaction-fixture-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name).resolve()
        self.service = self.root / "service"
        default_home = self.root / "default-home"
        default_home.mkdir(mode=0o700)
        self.default = default_home / ".solu"
        self.service.mkdir(mode=0o700)
        self.default.mkdir(mode=0o700)
        self.context_dir = self.service / ".migration-intents"
        self.context_dir.mkdir(mode=0o700)
        self.claim = b'{"machineId":"fixture-host","token":"synthetic-fixture-bearer"}'
        self.original = self.service / "claim.json"
        self.retired = self.service / "claim.json.retired-20261120"
        self.value = {
            "schema": 1, "migrationId": NONCE, "hostMachineId": "fixture-host", "wslMachineId": "fixture-host",
            "distro": "SoluTerminalRig", "linuxUser": self.user_name,
            "serviceDataDir": str(self.service), "defaultDataDir": str(self.default),
            "originalPath": str(self.original), "retiredPath": str(self.retired),
            "claimSha256": hashlib.sha256(self.claim).hexdigest(),
        }
        self.context_file = self.context_dir / (NONCE + ".json")
        private(self.context_file, self.value)
        private(self.original, self.claim)
        for directory in (self.service, self.default):
            private(directory / TX.IN_PROGRESS, {"schema": 1, "migrationId": NONCE})

    def run_tx(self, action, **kwargs):
        return TX.transaction(action, str(self.service), str(self.default), NONCE, **kwargs)

    def child(self, action, timeout="1"):
        return subprocess.run([sys.executable, "-I", "-c", CHILD_UNIT, str(SOURCE), action,
                               "--service-dir", str(self.service), "--default-dir", str(self.default),
                               "--migration-id", NONCE, "--timeout", timeout],
                              capture_output=True, timeout=5, check=False)

    def assert_original_untouched(self):
        self.assertEqual(self.original.read_bytes(), self.claim)
        self.assertFalse(self.retired.exists())

    def not_started(self):
        for directory in (self.service, self.default):
            (directory / TX.IN_PROGRESS).unlink()
        self.context_file.unlink()
        self.context_dir.rmdir()

    def test_preflight_is_read_only_before_any_prepare_write(self):
        self.not_started()
        real = os.open
        def readonly(path, flags, *args, **kwargs):
            self.assertFalse(flags & os.O_CREAT)
            return real(path, flags, *args, **kwargs)
        with mock.patch.object(TX.os, "open", side_effect=readonly), \
             mock.patch.object(TX.os, "fsync", side_effect=AssertionError("preflight fsync")), \
             mock.patch.object(TX.os, "unlink", side_effect=AssertionError("preflight unlink")):
            self.assertEqual(TX.preflight(str(self.service), str(self.default)), "available")
        self.assertFalse((self.service / ".migration.lock").exists())
        self.assertFalse(self.context_dir.exists())
        self.assert_original_untouched()

    def test_unsupported_preflight_is_unavailable_not_a_started_block(self):
        self.not_started()
        with mock.patch.object(TX, "native_filesystem", side_effect=TX.Refused):
            with self.assertRaises(TX.Unavailable):
                TX.preflight(str(self.service), str(self.default))
        self.assertFalse((self.service / ".migration.lock").exists())
        self.original.chmod(0o644)
        result = self.child("preflight")
        self.assertEqual(result.returncode, 69)
        self.assertIn(b"migration unavailable", result.stderr)
        self.assertNotIn(b"migration interrupted", result.stderr)
        self.assertFalse((self.service / ".migration.lock").exists())
        self.assert_original_untouched()

    def test_started_preflight_stays_blocked(self):
        self.assertEqual(self.child("preflight").returncode, 78)
        self.assertTrue((self.service / TX.IN_PROGRESS).exists())
        self.assertFalse((self.service / ".migration.lock").exists())


    def test_missing_default_during_started_preflight_is_blocked(self):
        marker = self.default / TX.IN_PROGRESS
        marker.unlink()
        self.default.rmdir()
        result = self.child("preflight")
        self.assertEqual(result.returncode, 78)
        self.assertIn(b"migration interrupted", result.stderr)
        self.assertTrue((self.service / TX.IN_PROGRESS).exists())
        self.assertFalse((self.service / ".migration.lock").exists())

    def test_unreadable_start_evidence_never_falls_back_to_unavailable(self):
        self.not_started()
        real = os.lstat
        def unreadable(path, *args, **kwargs):
            if str(path) == str(self.service / TX.IN_PROGRESS):
                raise PermissionError("fixture marker read denied")
            return real(path, *args, **kwargs)
        with mock.patch.object(TX, "native_filesystem", side_effect=TX.Refused), \
             mock.patch.object(TX.os, "lstat", side_effect=unreadable):
            with self.assertRaises(TX.Refused) as raised:
                TX.preflight(str(self.service), str(self.default))
        self.assertNotIsInstance(raised.exception, TX.Unavailable)
        self.assertFalse((self.service / ".migration.lock").exists())


    def test_before_invalidates_both_markers_and_late_child_refuses(self):
        self.assertEqual(self.run_tx("before"), "before")
        for directory in (self.service, self.default):
            self.assertFalse((directory / TX.IN_PROGRESS).exists())
        self.assertTrue(self.context_file.exists())
        late = self.child("rename")
        self.assertEqual(late.returncode, 78)
        self.assertNotIn(self.value["claimSha256"].encode(), late.stdout + late.stderr)
        self.assertNotIn(b"synthetic-fixture-bearer", late.stdout + late.stderr)
        self.assert_original_untouched()
        self.assertEqual(self.run_tx("before"), "before")  # ACK lost after successful clear.

    def test_rename_is_no_replace_and_syncs_before_reached(self):
        calls = []
        real = os.fsync
        with mock.patch.object(TX.os, "fsync", side_effect=lambda fd: (calls.append(fd), real(fd))[1]):
            self.assertEqual(self.run_tx("rename"), "reached")
        self.assertFalse(self.original.exists())
        self.assertEqual(self.retired.read_bytes(), self.claim)
        self.assertGreaterEqual(len(calls), 4)  # Lock, dir, claim, renamed directory.
        self.assertTrue((self.service / TX.IN_PROGRESS).exists())
        with self.assertRaises(Exception):
            self.run_tx("before")
        self.assertEqual(self.retired.read_bytes(), self.claim)

    def test_target_appearing_at_syscall_is_never_replaced(self):
        real = TX.LIBC.renameat2
        real.argtypes = [TX.ctypes.c_int, TX.ctypes.c_char_p, TX.ctypes.c_int, TX.ctypes.c_char_p, TX.ctypes.c_uint]
        real.restype = TX.ctypes.c_int
        def race(source_fd, source, target_fd, target, flags):
            private(self.retired, b"late unrelated fixture")
            return real(source_fd, source, target_fd, target, flags)
        with mock.patch.object(TX.LIBC, "renameat2", race):
            with self.assertRaises(TX.Refused):
                self.run_tx("rename")
        self.assertEqual(self.original.read_bytes(), self.claim)
        self.assertEqual(self.retired.read_bytes(), b"late unrelated fixture")

    def test_lost_rename_sync_ack_never_becomes_before(self):
        real = os.fsync
        count = 0
        def fail_after_rename(fd):
            nonlocal count
            if os.fstat(fd).st_ino == self.service.stat().st_ino:
                count += 1
                if count == 2:
                    raise OSError("fixture failure after rename")
            return real(fd)
        with mock.patch.object(TX.os, "fsync", side_effect=fail_after_rename):
            with self.assertRaises(OSError):
                self.run_tx("rename")
        self.assertFalse(self.original.exists())
        self.assertEqual(self.retired.read_bytes(), self.claim)
        self.assertEqual(self.child("before").returncode, 78)
        self.assertTrue((self.service / TX.IN_PROGRESS).exists())
        self.assertTrue(self.context_file.exists())


    def test_existing_retired_target_and_fences_never_change(self):
        private(self.retired, b"unrelated fixture")
        for action in ("rename", "before"):
            self.assertEqual(self.child(action).returncode, 78)
        self.assertEqual(self.retired.read_bytes(), b"unrelated fixture")
        self.assertEqual(self.original.read_bytes(), self.claim)
        self.retired.unlink()  # Fixture-only pre-existing target.
        for directory in (self.service, self.default):
            private(directory / TX.RETIRED, b"")
        self.assertEqual(self.child("before").returncode, 78)
        for directory in (self.service, self.default):
            self.assertTrue((directory / TX.RETIRED).exists())
            self.assertTrue((directory / TX.IN_PROGRESS).exists())

    def test_nonce_mismatch_never_partially_clears(self):
        marker = self.default / TX.IN_PROGRESS
        marker.write_text(json.dumps({"schema": 1, "migrationId": "b" * 32}))
        for action in ("rename", "before"):
            self.assertEqual(self.child(action).returncode, 78)
        self.assertTrue((self.service / TX.IN_PROGRESS).exists())
        self.assertTrue(marker.exists())
        self.assert_original_untouched()

    def test_held_lock_times_out_without_any_claim_action(self):
        path = self.service / ".migration.lock"
        private(path, b"")
        with path.open("rb") as held:
            fcntl.flock(held, fcntl.LOCK_EX)
            self.assertEqual(self.child("before", "0.05").returncode, 78)
        self.assert_original_untouched()
        self.assertTrue((self.service / TX.IN_PROGRESS).exists())

    def test_crash_releases_lock_and_next_start_revalidates(self):
        path = self.service / ".migration.lock"
        private(path, b"")
        script = "import fcntl,sys; f=open(sys.argv[1],'rb'); fcntl.flock(f,fcntl.LOCK_EX); print('locked',flush=True); sys.stdin.read()"
        with subprocess.Popen([sys.executable, "-I", "-c", script, str(path)], stdin=subprocess.PIPE, stdout=subprocess.PIPE) as child:
            try:
                self.assertTrue(select.select([child.stdout], [], [], 3)[0])
                self.assertEqual(child.stdout.readline(), b"locked\n")
            finally:
                child.kill()
                child.wait(timeout=3)
        self.assertEqual(self.run_tx("before"), "before")
        self.assert_original_untouched()

    def test_partial_clear_keeps_private_context_for_retry(self):
        real = os.unlink
        def fail_default(name, *, dir_fd=None):
            if name == TX.IN_PROGRESS and os.fstat(dir_fd).st_ino == self.default.stat().st_ino:
                raise OSError("fixture unlink failure")
            return real(name, dir_fd=dir_fd)
        with mock.patch.object(TX.os, "unlink", side_effect=fail_default):
            with self.assertRaises(OSError):
                self.run_tx("before")
        self.assertFalse((self.service / TX.IN_PROGRESS).exists())
        self.assertTrue((self.default / TX.IN_PROGRESS).exists())
        self.assertTrue(self.context_file.exists())
        self.assertEqual(self.run_tx("before"), "before")
        self.assertEqual(self.child("rename").returncode, 78)

    def test_dir_sync_failure_is_blocked_and_retry_syncs_absent_markers(self):
        real = os.fsync
        def fail_default(fd):
            if os.fstat(fd).st_ino == self.default.stat().st_ino:
                raise OSError("fixture dir sync failure")
            return real(fd)
        with mock.patch.object(TX.os, "fsync", side_effect=fail_default):
            with self.assertRaises(OSError):
                self.run_tx("before")
        calls = []
        with mock.patch.object(TX.os, "fsync", side_effect=lambda fd: (calls.append(os.fstat(fd).st_ino), real(fd))[1]):
            self.assertEqual(self.run_tx("before"), "before")
        self.assertIn(self.default.stat().st_ino, calls)
        self.assert_original_untouched()

    def test_changed_claim_or_public_context_refuses_without_leaking_values(self):
        self.original.write_bytes(b"changed synthetic fixture")
        result = self.child("rename")
        self.assertEqual(result.returncode, 78)
        self.assertNotIn(self.value["claimSha256"].encode(), result.stdout + result.stderr)
        self.assertNotIn(b"changed synthetic fixture", result.stdout + result.stderr)
        self.original.write_bytes(self.claim)
        self.context_file.chmod(0o644)
        self.assertEqual(self.child("before").returncode, 78)
        self.assert_original_untouched()

    def test_symlink_lock_and_claim_refuse_without_following(self):
        foreign = self.root / "foreign"
        private(foreign, b"unrelated synthetic fixture")
        lock = self.service / ".migration.lock"
        lock.symlink_to(foreign)
        self.assertEqual(self.child("before").returncode, 78)
        self.assertEqual(foreign.read_bytes(), b"unrelated synthetic fixture")
        lock.unlink()
        self.original.unlink()
        self.original.symlink_to(foreign)
        self.assertEqual(self.child("rename").returncode, 78)
        self.assertEqual(foreign.read_bytes(), b"unrelated synthetic fixture")

    def test_unsupported_filesystem_refuses_before_lock_creation(self):
        with mock.patch.object(TX, "native_filesystem", side_effect=TX.Refused):
            with self.assertRaises(TX.Refused):
                self.run_tx("before")
        self.assertFalse((self.service / ".migration.lock").exists())
        self.assert_original_untouched()

    def test_distinct_context_or_unknown_fields_are_not_shared_rename_authority(self):
        self.value["wslMachineId"] = "other-machine"
        self.context_file.write_text(json.dumps(self.value))
        self.assertEqual(self.child("rename").returncode, 78)
        self.value["wslMachineId"] = self.value["hostMachineId"]
        self.value["unknown"] = "private fixture value"
        self.context_file.write_text(json.dumps(self.value))
        result = self.child("before")
        self.assertEqual(result.returncode, 78)
        self.assertNotIn(b"private fixture value", result.stdout + result.stderr)
        self.assert_original_untouched()


class FilesystemPolicy(unittest.TestCase):
    def check_magic(self, magic):
        if TX.ctypes.sizeof(TX.ctypes.c_long) != 8:
            self.skipTest("production filesystem policy supports 64-bit Linux only")
        def statfs(_fd, info):
            TX.ctypes.c_long.from_buffer(info).value = magic
            return 0
        with mock.patch.object(TX.LIBC, "fstatfs", statfs):
            TX.native_filesystem(123)

    def test_native_ext4_policy_accepts_only_ext4(self):
        self.check_magic(0xEF53)
        for magic in (0x5346544E, 0x01021997, 0x01021994, 0x794C7630, 0x9123683E):
            with self.subTest(magic=magic), self.assertRaises(TX.Refused):
                self.check_magic(magic)

    def test_missing_statfs_uses_the_normal_unsupported_path(self):
        with mock.patch.object(TX.LIBC, "fstatfs", None), self.assertRaises(TX.Refused):
            TX.native_filesystem(123)

    def test_statfs_failure_refuses(self):
        def failure(_fd, _info):
            return -1
        with mock.patch.object(TX.LIBC, "fstatfs", failure), self.assertRaises(TX.Refused):
            TX.native_filesystem(123)


if __name__ == "__main__":
    unittest.main()
