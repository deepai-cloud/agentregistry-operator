#!/usr/bin/env python3
"""Exercise local credential creation and failures without contacting a cluster."""

from contextlib import redirect_stderr, redirect_stdout
import io
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "examples/local/create-secrets.py"
NAMES = ("secrets.json", "credentials.txt", "curl.conf")
create = runpy.run_path(str(SCRIPT))["main"]


class CreateSecretsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="areg-secrets-test-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name) / "credentials"

    def generate(self):
        with patch.object(sys, "argv", [str(SCRIPT), "--output-dir", str(self.directory)]), \
                redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
            create()

    def test_complete_private_files_and_no_credentials_in_output(self):
        for external in (False, True):
            with self.subTest(external=external):
                directory = self.directory / str(external)
                command = [sys.executable, str(SCRIPT), "--output-dir", str(directory)]
                if external:
                    command.append("--external-database")
                result = subprocess.run(command, text=True, capture_output=True, timeout=10, umask=0)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertCountEqual([p.name for p in directory.iterdir()], NAMES)
                self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
                for name in NAMES:
                    self.assertEqual((directory / name).stat().st_mode & 0o777, 0o600)
                items = json.loads((directory / "secrets.json").read_text())["items"]
                self.assertEqual(len(items), 3 if external else 1)
                auth = items[0]["stringData"]
                self.assertEqual((directory / "credentials.txt").read_text(),
                                 f"Username: {auth['username']}\nPassword: {auth['password']}\n")
                self.assertEqual((directory / "curl.conf").read_text(),
                                 f'user = "{auth["username"]}:{auth["password"]}"\n')
                for item in items:
                    for value in item["stringData"].values():
                        if value != auth["username"]:
                            self.assertNotIn(value, result.stdout + result.stderr)

    def test_write_failure_leaves_no_partial_set_and_can_retry(self):
        real_fdopen = os.fdopen
        writes = 0

        def fail_write(descriptor, *args, **kwargs):
            nonlocal writes
            stream = real_fdopen(descriptor, *args, **kwargs)
            writes += 1
            if writes == 2:
                # Fail after writing partial bytes, not merely before open.
                def interrupted_write(value):
                    stream.buffer.write(value[:5].encode())
                    raise OSError("simulated write failure")
                stream.write = interrupted_write
            return stream

        with patch("os.fdopen", side_effect=fail_write), self.assertRaises(OSError):
            self.generate()
        self.assertEqual(list(self.directory.iterdir()), [])
        self.generate()
        self.assertCountEqual([p.name for p in self.directory.iterdir()], NAMES)

    def test_existing_files_and_dangling_links_are_preserved(self):
        root = self.directory
        for name in NAMES:
            for symlink in (False, True):
                with self.subTest(name=name, symlink=symlink):
                    self.directory = root / (name + str(symlink))
                    self.directory.mkdir(parents=True)
                    existing = self.directory / name
                    if symlink:
                        existing.symlink_to("missing-target")
                    else:
                        existing.write_text("original content")
                    try:
                        with self.assertRaises(SystemExit) as raised:
                            self.generate()
                        self.assertEqual(raised.exception.code, 2)
                        self.assertEqual(list(self.directory.iterdir()), [existing])
                        if symlink:
                            self.assertEqual(os.readlink(existing), "missing-target")
                        else:
                            self.assertEqual(existing.read_text(), "original content")
                    finally:
                        existing.unlink()

    def test_publish_failure_removes_only_this_attempts_files(self):
        real_link = os.link
        for failure in (OSError, KeyboardInterrupt, FileExistsError):
            with self.subTest(failure=failure):
                def fail_last(source, destination):
                    destination = Path(destination)
                    if destination.name == "secrets.json":
                        self.assertTrue((self.directory / "credentials.txt").is_file())
                        self.assertTrue((self.directory / "curl.conf").is_file())
                        if failure is FileExistsError:
                            destination.write_text("another writer's manifest")
                        raise failure("simulated publish failure")
                    return real_link(source, destination)

                with patch("os.link", side_effect=fail_last), self.assertRaises(failure):
                    self.generate()
                if failure is FileExistsError:
                    manifest = self.directory / "secrets.json"
                    self.assertEqual(list(self.directory.iterdir()), [manifest])
                    self.assertEqual(manifest.read_text(), "another writer's manifest")
                    manifest.unlink()
                else:
                    self.assertEqual(list(self.directory.iterdir()), [])
        self.generate()
        self.assertCountEqual([p.name for p in self.directory.iterdir()], NAMES)


if __name__ == "__main__":
    unittest.main()
