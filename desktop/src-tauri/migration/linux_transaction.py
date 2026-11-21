"""Private W5 resource. Explicit paths only; no helper/daemon/cloud interface.

The Windows coordinator must already have joined the mirror and stopped the
Linux service. Claim fingerprints are read only from a private context file.
"""
import argparse
import contextlib
import ctypes
import errno
import fcntl
import hashlib
import hmac
import json
import os
import pwd
import re
import stat
import sys
import time

BLOCKED = "migration interrupted: sign in to finish, or contact support"
IN_PROGRESS = "migration-in-progress.json"
RETIRED = "retired-to-host.json"
NOFOLLOW = os.O_NOFOLLOW | os.O_CLOEXEC
LIBC = ctypes.CDLL(None, use_errno=True)


class Refused(Exception):
    pass


def require(condition):
    if not condition:
        raise Refused()


def native_filesystem(fd):
    # A Windows host uses fixed NTFS. Requiring native Linux ext4 here also
    # prevents a /mnt/c alias from renaming the host's own claim. No probing or
    # mutation of an unsupported mount is attempted beyond opening the dir.
    require(sys.platform == "linux" and ctypes.sizeof(ctypes.c_long) == 8)
    info = ctypes.create_string_buffer(512)
    call = getattr(LIBC, "fstatfs", None)
    require(call is not None)
    call.argtypes = [ctypes.c_int, ctypes.c_void_p]
    call.restype = ctypes.c_int
    require(call(fd, info) == 0)
    require(ctypes.c_long.from_buffer(info).value == 0xEF53)


class Directory:
    def __init__(self, path):
        require(isinstance(path, str) and path.startswith("/") and os.path.realpath(path) == path)
        self.path = path
        self.fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW)
        try:
            info = os.fstat(self.fd)
            require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid() and not info.st_mode & 0o022)
            native_filesystem(self.fd)
            self.identity = (info.st_dev, info.st_ino)
        except BaseException:
            os.close(self.fd)
            raise

    def close(self):
        os.close(self.fd)

    def exists(self, name):
        try:
            os.stat(name, dir_fd=self.fd, follow_symlinks=False)
            return True
        except FileNotFoundError:
            return False

    def open_private(self, name):
        fd = os.open(name, os.O_RDONLY | os.O_NONBLOCK | NOFOLLOW, dir_fd=self.fd)
        try:
            info = os.fstat(fd)
            require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
                    and info.st_nlink == 1 and not info.st_mode & 0o077)
            native_filesystem(fd)
            return fd
        except BaseException:
            os.close(fd)
            raise

    def private_bytes(self, name, limit):
        fd = self.open_private(name)
        try:
            with os.fdopen(fd, "rb", closefd=False) as stream:
                data = stream.read(limit + 1)
            require(len(data) <= limit)
            return data
        finally:
            os.close(fd)


def no_duplicates(pairs):
    value = {}
    for key, item in pairs:
        require(key not in value)
        value[key] = item
    return value


def decode(data):
    value = json.loads(data, object_pairs_hook=no_duplicates)
    require(isinstance(value, dict))
    return value


def context(service, default_path, nonce):
    with contextlib.ExitStack() as stack:
        private = Directory(service.path + "/.migration-intents")
        stack.callback(private.close)
        info = os.fstat(private.fd)
        require(not info.st_mode & 0o077)
        value = decode(private.private_bytes(nonce + ".json", 16384))
    keys = {"schema", "migrationId", "hostMachineId", "wslMachineId", "distro", "linuxUser",
            "serviceDataDir", "defaultDataDir", "originalPath", "retiredPath", "claimSha256"}
    require(set(value) == keys and type(value["schema"]) is int and value["schema"] == 1)
    require(value["migrationId"] == nonce and value["serviceDataDir"] == service.path
            and value["defaultDataDir"] == default_path and default_path.endswith("/.solu"))
    machine = value["hostMachineId"]
    require(isinstance(machine, str) and re.fullmatch(r"[A-Za-z0-9_:-]{1,128}", machine)
            and value["wslMachineId"] == machine)
    require(value["linuxUser"] == pwd.getpwuid(os.geteuid()).pw_name)
    require(isinstance(value["distro"], str) and 0 < len(value["distro"]) <= 256
            and all(ord(c) >= 32 and ord(c) != 127 for c in value["distro"]))
    require(value["originalPath"] == service.path + "/claim.json")
    retired = value["retiredPath"]
    prefix = service.path + "/claim.json.retired-"
    require(isinstance(retired, str) and retired.startswith(prefix)
            and re.fullmatch(r"[0-9TZ-]{1,64}", retired[len(prefix):]))
    require(isinstance(value["claimSha256"], str) and re.fullmatch(r"[0-9a-f]{64}", value["claimSha256"]))
    return value, retired[len(service.path) + 1:]


def matching_marker(directory, nonce, allow_absent=False):
    if not directory.exists(IN_PROGRESS):
        require(allow_absent)
        return False
    marker = decode(directory.private_bytes(IN_PROGRESS, 4096))
    require(set(marker) == {"schema", "migrationId"} and type(marker["schema"]) is int
            and marker["schema"] == 1 and marker["migrationId"] == nonce)
    return True


def matching_claim(service, value):
    fd = service.open_private("claim.json")
    try:
        with os.fdopen(fd, "rb", closefd=False) as stream:
            data = stream.read(65537)
        require(len(data) <= 65536 and hmac.compare_digest(hashlib.sha256(data).hexdigest(), value["claimSha256"]))
        require(decode(data).get("machineId") == value["wslMachineId"])
        opened = os.fstat(fd)
        named = os.stat("claim.json", dir_fd=service.fd, follow_symlinks=False)
        require((opened.st_dev, opened.st_ino) == (named.st_dev, named.st_ino))
        return fd
    except BaseException:
        os.close(fd)
        raise


@contextlib.contextmanager
def locked(service, timeout):
    fd = os.open(".migration.lock", os.O_RDWR | os.O_CREAT | NOFOLLOW, 0o600, dir_fd=service.fd)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
                and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o600)
        native_filesystem(fd)
        os.fsync(fd)
        os.fsync(service.fd)
        deadline = time.monotonic() + timeout
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                require(time.monotonic() < deadline)
                time.sleep(min(0.01, max(0, deadline - time.monotonic())))
        named = os.stat(".migration.lock", dir_fd=service.fd, follow_symlinks=False)
        require((info.st_dev, info.st_ino) == (named.st_dev, named.st_ino))
        yield
    finally:
        os.close(fd)  # flock is released even on a crash/failed action.


class Unavailable(Refused):
    def __init__(self, reason):
        self.reason = reason  # Only fixed, non-secret reasons below.


def preflight(service_path, default_path):
    """Read-only capability check BEFORE any context/intent/lock publication."""
    try:
        require(default_path.endswith("/.solu"))
        with contextlib.ExitStack() as stack:
            service = Directory(service_path)
            stack.callback(service.close)
            default = Directory(default_path)
            stack.callback(default.close)
            directories = [service] if service.identity == default.identity else [service, default]
            for directory in directories:
                require(os.fstat(directory.fd).st_mode & 0o300 == 0o300)
                require(not os.fstatvfs(directory.fd).f_flag & os.ST_RDONLY)
                if directory.exists(RETIRED):
                    raise Unavailable("Dual was already retired")
                if directory.exists(IN_PROGRESS):
                    raise Refused()  # A transaction has already started.
            claim = service.open_private("claim.json")
            stack.callback(os.close, claim)
            # Check the actual syscall without a writable path or valid dirfd.
            rename = getattr(LIBC, "renameat2", None)
            require(rename is not None)
            rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
            rename.restype = ctypes.c_int
            ctypes.set_errno(0)
            require(rename(-1, b"migration-preflight", -1, b"migration-preflight-target", 1) == -1
                    and ctypes.get_errno() == errno.EBADF)
            if service.exists(".migration-intents"):
                private = Directory(service.path + "/.migration-intents")
                stack.callback(private.close)
                require(stat.S_IMODE(os.fstat(private.fd).st_mode) == 0o700)
            if service.exists(".migration.lock"):
                # Never O_CREAT or fsync during preflight. A pre-existing lock
                # must already have the required identity and private mode.
                lock = os.open(".migration.lock", os.O_RDWR | NOFOLLOW, dir_fd=service.fd)
                stack.callback(os.close, lock)
                info = os.fstat(lock)
                require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
                        and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o600)
                native_filesystem(lock)
            return "available"
    except Unavailable:
        raise
    except (Refused, OSError) as error:
        # Any positive marker or unreadable marker state is a started/unknown
        # transaction. Only proven absence can report an unstarted unavailable.
        uncertain = False
        for path in (service_path, default_path):
            try:
                os.lstat(os.path.join(path, IN_PROGRESS))
            except (FileNotFoundError, NotADirectoryError):
                continue
            except OSError:
                uncertain = True
            else:
                raise Refused() from None
        if uncertain:
            raise Refused() from None
        reason = ("Linux data folders and private files must be accessible" if isinstance(error, OSError)
                  else "native ext4 folders, private files and renameat2 are required")
        raise Unavailable(reason) from None


def transaction(action, service_path, default_path, nonce, timeout=5):
    require(action in ("rename", "before") and isinstance(nonce, str)
            and re.fullmatch(r"[0-9a-f]{32}", nonce) and 0 < timeout <= 10)
    with contextlib.ExitStack() as stack:
        service = Directory(service_path)
        stack.callback(service.close)
        default = Directory(default_path)
        stack.callback(default.close)
        directories = [service] if service.identity == default.identity else [service, default]
        with locked(service, timeout):
            value, retired = context(service, default.path, nonce)
            require(not service.exists(retired))
            for directory in directories:
                require(not directory.exists(RETIRED))
            fd = matching_claim(service, value)
            stack.callback(os.close, fd)
            if action == "rename":
                for directory in directories:
                    matching_marker(directory, nonce)
                os.fsync(fd)
                rename = LIBC.renameat2
                rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
                rename.restype = ctypes.c_int
                require(rename(service.fd, b"claim.json", service.fd, os.fsencode(retired), 1) == 0)  # RENAME_NOREPLACE
                os.fsync(service.fd)
                return "reached"
            markers = [matching_marker(directory, nonce, allow_absent=True) for directory in directories]
            # Invalidate the service nonce first. The same lock prevents a late
            # rename from entering until all deletions are durable; a retry is
            # possible using the retained private context after a partial clear.
            for directory, present in zip(directories, markers):
                if present:
                    os.unlink(IN_PROGRESS, dir_fd=directory.fd)
                # Also finish a previous unlink whose ACK/dir sync was lost.
                os.fsync(directory.fd)
            return "before"


class Arguments(argparse.ArgumentParser):
    def error(self, _message):
        raise Refused()  # Never echo an unexpected argument or private value.


def main():
    try:
        parser = Arguments(add_help=False)
        parser.add_argument("action", choices=("preflight", "rename", "before"))
        parser.add_argument("--service-dir", required=True)
        parser.add_argument("--default-dir", required=True)
        parser.add_argument("--migration-id", required=True)
        parser.add_argument("--timeout", type=float, default=5)
        args = parser.parse_args()
        require(re.fullmatch(r"[0-9a-f]{32}", args.migration_id))
        if args.action == "preflight":
            result = preflight(args.service_dir, args.default_dir)
        else:
            result = transaction(args.action, args.service_dir, args.default_dir, args.migration_id, args.timeout)
        print(result)
        return 0
    except Unavailable as error:
        print("migration unavailable: " + error.reason, file=sys.stderr)
        return 69
    except Exception:
        print(BLOCKED, file=sys.stderr)
        return 78


if __name__ == "__main__":
    sys.exit(main())
