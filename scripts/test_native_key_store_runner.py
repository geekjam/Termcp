"""Credential fixtures must be restored when native tests or setup fail."""

from pathlib import Path
import tempfile
import unittest
from unittest import mock

import test_native_key_store as runner


class NativeKeyStoreRunnerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.default = self.root / "original.keychain-db"
        self.search = [self.default, self.root / "other keychain.keychain-db"]
        self.fixture = self.root / "native test.keychain-db"
        self.commands = []

    def checked(self, args, **kwargs):
        self.commands.append((args, kwargs))
        if args[1:] == ["default-keychain", "-d", "user"]:
            return ('"' + str(self.default) + '"\n').encode()
        if args[1:] == ["list-keychains", "-d", "user"]:
            return "\n".join('"' + str(path) + '"' for path in self.search).encode()
        if args[1:] == ["-i"]:
            self.fixture.touch()
        return b""

    def assert_restored(self):
        commands = [args for args, _ in self.commands]
        self.assertIn([runner.SECURITY, "default-keychain", "-d", "user", "-s", str(self.default)], commands)
        self.assertIn([runner.SECURITY, "list-keychains", "-d", "user", "-s", *map(str, self.search)], commands)
        self.assertIn([runner.SECURITY, "delete-keychain", str(self.fixture)], commands)

    def test_test_failure_restores_keychain_and_keeps_password_out_of_argv(self):
        password = "generated-fixture-password"
        with mock.patch.object(runner, "checked", side_effect=self.checked), mock.patch.object(runner.secrets, "token_hex", return_value=password):
            with self.assertRaisesRegex(RuntimeError, "test failure"):
                with runner.mac_keychain(self.root):
                    raise RuntimeError("test failure")
        self.assert_restored()
        for args, _ in self.commands:
            self.assertNotIn(password, " ".join(args))
        create = next(kwargs for args, kwargs in self.commands if args[1:] == ["-i"])
        self.assertIn(password.encode(), create["input"])

    def test_failed_default_restore_does_not_skip_other_cleanup(self):
        def checked(args, **kwargs):
            result = self.checked(args, **kwargs)
            if args[1:] == ["default-keychain", "-d", "user", "-s", str(self.default)]:
                raise RuntimeError("restore failure")
            return result

        with mock.patch.object(runner, "checked", side_effect=checked):
            with self.assertRaisesRegex(RuntimeError, "restore"):
                with runner.mac_keychain(self.root):
                    pass
        self.assert_restored()

    def test_secret_service_start_failure_stops_daemon_and_closes_stdin(self):
        daemon = mock.Mock()
        daemon.poll.return_value = None
        daemon.stdin.closed = False
        daemon.stdin.close.side_effect = lambda: setattr(daemon.stdin, "closed", True)
        env = {"PATH": "test-path"}
        with mock.patch.object(runner.subprocess, "Popen", return_value=daemon), mock.patch.object(runner, "checked", side_effect=RuntimeError("unavailable")):
            with self.assertRaisesRegex(RuntimeError, "unavailable"):
                with runner.linux_keyring(self.root, env):
                    self.fail("an unavailable service must fail setup")
        daemon.stdin.close.assert_called_once()
        daemon.terminate.assert_called_once()
        daemon.wait.assert_called_once_with(timeout=5)
        self.assertNotIn("HOME", env)
        for key in ("XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"):
            self.assertTrue(Path(env[key]).is_relative_to(self.root))

    def test_failed_native_test_remains_a_failed_job(self):
        process = mock.Mock()
        process.wait.return_value = 1
        process.poll.return_value = 1
        with mock.patch.object(runner.subprocess, "Popen", return_value=process):
            self.assertEqual(runner.run_tests({}), 1)
        process.terminate.assert_not_called()


if __name__ == "__main__":
    unittest.main()
