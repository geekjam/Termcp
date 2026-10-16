"""Run real OS key-store tests in disposable credential fixtures.

macOS temporarily selects a fresh Keychain, restoring the previous default and
search list even when tests fail. Linux uses a private D-Bus session and XDG
directories. Windows DPAPI needs no external credential database.
"""

from contextlib import contextmanager
import json
import os
from pathlib import Path
import secrets
import shlex
import signal
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[1]
SECURITY = "/usr/bin/security"


def checked(args, *, input=None, env=None):
    result = subprocess.run(
        args, input=input, env=env, stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, timeout=30, check=False,
    )
    if result.returncode:
        # Never include command input/output, credentials or local paths.
        raise RuntimeError("native credential fixture command failed")
    return result.stdout


def security_quote(value):
    if any(character in str(value) for character in "\r\n\0"):
        raise RuntimeError("invalid native fixture path")
    return json.dumps(str(value), ensure_ascii=False)


@contextmanager
def mac_keychain(root):
    previous_default = shlex.split(checked([SECURITY, "default-keychain", "-d", "user"]).decode())
    previous_search = shlex.split(checked([SECURITY, "list-keychains", "-d", "user"]).decode())
    if len(previous_default) != 1:
        raise RuntimeError("cannot safely preserve the current default Keychain")
    keychain = root / "native test.keychain-db"
    password = secrets.token_hex(32)
    try:
        # security's interactive parser gets the password on stdin, not argv.
        command = f"create-keychain -p {password} {security_quote(keychain)}\n"
        checked([SECURITY, "-i"], input=command.encode())
        password = command = None
        checked([SECURITY, "show-keychain-info", str(keychain)])
        checked([SECURITY, "default-keychain", "-d", "user", "-s", str(keychain)])
        checked([SECURITY, "list-keychains", "-d", "user", "-s", str(keychain)])
        yield
    finally:
        failures = []
        commands = [
            [SECURITY, "default-keychain", "-d", "user", "-s", *previous_default],
            [SECURITY, "list-keychains", "-d", "user", "-s", *previous_search],
        ]
        if keychain.exists():
            commands.append([SECURITY, "delete-keychain", str(keychain)])
        for args in commands:
            try:
                checked(args)
            except (RuntimeError, OSError, subprocess.TimeoutExpired):
                failures.append(True)
        if failures:
            raise RuntimeError("could not completely restore the Keychain fixture")


def stop(process):
    if process.poll() is None:
        try:
            process.terminate()
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


@contextmanager
def linux_keyring(root, env):
    for variable, directory in (
        ("XDG_DATA_HOME", "data"), ("XDG_CONFIG_HOME", "config"),
        ("XDG_CACHE_HOME", "cache"), ("XDG_RUNTIME_DIR", "runtime"),
    ):
        location = root / directory
        location.mkdir(mode=0o700)
        env[variable] = str(location)
    control = root / "control"
    control.mkdir(mode=0o700)
    daemon = subprocess.Popen(
        ["gnome-keyring-daemon", "--foreground", "--components=secrets",
         "--unlock", "--control-directory=" + str(control)],
        stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL, env=env,
    )
    try:
        daemon.stdin.write(secrets.token_hex(32).encode())
        daemon.stdin.close()
        checked(["gdbus", "wait", "--session", "--timeout", "20", "org.freedesktop.secrets"], env=env)
        if daemon.poll() is not None:
            raise RuntimeError("isolated Secret Service stopped before the tests")
        yield
    finally:
        if daemon.stdin and not daemon.stdin.closed:
            daemon.stdin.close()
        stop(daemon)


def run_tests(env):
    process = subprocess.Popen(
        ["go", "test", "./internal/encryptedfs", "-count=1", "-shuffle=on",
         "-race", "-timeout", "180s", "-v", "-run", "^TestSystemProviderNative"],
        cwd=ROOT, env=env,
    )
    try:
        return process.wait(timeout=240)
    finally:
        stop(process)


def main():
    if sys.platform == "linux" and sys.argv[1:] != ["--linux-session"]:
        # dbus-run-session reaps its private bus when the inner process exits.
        return subprocess.call(["dbus-run-session", "--", sys.executable, str(Path(__file__).resolve()), "--linux-session"])
    if sys.platform not in ("darwin", "linux", "win32"):
        raise RuntimeError("native key-store integration is unsupported on this OS")
    with tempfile.TemporaryDirectory(prefix="termcp-native-credentials-") as directory:
        root = Path(directory)
        env = os.environ.copy()
        env.pop("TERMCP_TEST_NATIVE_CHILD", None)
        env["TERMCP_TEST_NATIVE_KEYSTORE"] = "1"
        env["TERMCP_TEST_KEYSTORE_FIXTURE"] = str(root)
        if sys.platform == "darwin":
            with mac_keychain(root):
                return run_tests(env)
        if sys.platform == "linux":
            with linux_keyring(root, env):
                return run_tests(env)
        return run_tests(env)


if __name__ == "__main__":
    # Give fixture cleanup a chance on runner cancellation as well as Ctrl-C.
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(130))
    try:
        sys.exit(main())
    except (RuntimeError, OSError, subprocess.SubprocessError):
        print("Native key-store test setup or cleanup failed.", file=sys.stderr)
        sys.exit(1)
