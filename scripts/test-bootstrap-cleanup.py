#!/usr/bin/env python3
"""Exercise bootstrap resource cleanup without contacting Docker."""

import importlib.util
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "postgres_bootstrap", Path(__file__).with_name("test-postgres-bootstrap.py"))
bootstrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bootstrap)


class Docker:
    """Model daemon state independently of whether the create client succeeds."""

    def __init__(self, failure=None):
        self.failure = failure
        self.containers = {}
        self.calls = []
        self.owned_id = "a" * 64
        self.foreign_id = "b" * 64

    def foreign(self, name):
        self.containers[self.foreign_id] = {"name": name, "labels": {}}

    def __call__(self, *args, **kwargs):
        self.calls.append(args)
        command = args[0]
        output = ""
        if command == "create":
            name = args[args.index("--name") + 1]
            if self.failure == "collision":
                self.foreign(name)
                raise RuntimeError("Docker create failed (exit 1)")
            if self.failure == "before-create":
                raise RuntimeError("Docker create failed (exit 1)")
            labels = {}
            for index, value in enumerate(args[:-1]):
                if value == "--label":
                    key, value = args[index + 1].split("=", 1)
                    labels[key] = value
            self.containers[self.owned_id] = {"name": name, "labels": labels}
            if self.failure == "timeout":
                raise subprocess.TimeoutExpired("docker create", 180)
            if self.failure == "interrupted":
                raise KeyboardInterrupt()
            output = self.owned_id + "\n"
        elif command == "ps":
            assert "--all" in args and "--quiet" in args and "--no-trunc" in args
            selector = args[args.index("--filter") + 1]
            assert selector.startswith("label=")
            key, value = selector.removeprefix("label=").split("=", 1)
            assert value, "ownership token must not be empty"
            output = "\n".join(identifier for identifier, container in self.containers.items()
                               if container["labels"].get(key) == value)
        elif command == "rm":
            assert args[1:3] == ("--force", "--volumes"), "anonymous volumes must be removed"
            for identifier in args[3:]:
                # Accept names too so the old implementation's unsafe behavior
                # is observable when an unrelated container reuses the name.
                resolved = next((key for key, value in self.containers.items()
                                 if key == identifier or value["name"] == identifier), None)
                if resolved:
                    del self.containers[resolved]
        elif command == "start" and self.failure == "start":
            raise RuntimeError("Docker start failed (exit 1)")
        elif command not in ("cp", "start"):
            raise AssertionError(f"Unexpected Docker command: {command}")
        return SimpleNamespace(returncode=0, stdout=output, stderr="")


class BootstrapCleanupTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="areg-bootstrap-cleanup-")
        self.addCleanup(temporary.cleanup)
        self.work = Path(temporary.name)

    def test_success_removes_container_and_anonymous_volumes_by_id(self):
        daemon = Docker()
        with patch.object(bootstrap, "docker", daemon):
            with bootstrap.Database(self.work, bootstrap.INIT):
                self.assertIn(daemon.owned_id, daemon.containers)
        self.assertEqual(daemon.containers, {})
        self.assertIn(("rm", "--force", "--volumes", daemon.owned_id), daemon.calls)

    def test_ambiguous_create_failure_cleans_up_and_preserves_original_error(self):
        for failure, error in (("timeout", subprocess.TimeoutExpired), ("interrupted", KeyboardInterrupt)):
            with self.subTest(failure=failure):
                daemon = Docker(failure)
                with patch.object(bootstrap, "docker", daemon), self.assertRaises(error):
                    with bootstrap.Database(self.work, bootstrap.INIT):
                        self.fail("failed create must not enter the test")
                self.assertEqual(daemon.containers, {}, "daemon-created container leaked after client failure")

    def test_create_failure_before_container_exists_does_not_remove_anything(self):
        for failure in ("before-create", "collision"):
            with self.subTest(failure=failure):
                daemon = Docker(failure)
                with patch.object(bootstrap, "docker", daemon), self.assertRaisesRegex(RuntimeError, "create failed"):
                    with bootstrap.Database(self.work, bootstrap.INIT):
                        self.fail("failed create must not enter the test")
                self.assertFalse(any(call[0] == "rm" for call in daemon.calls))
                if failure == "collision":
                    self.assertIn(daemon.foreign_id, daemon.containers)

    def test_start_failure_removes_the_created_container(self):
        daemon = Docker("start")
        with patch.object(bootstrap, "docker", daemon), self.assertRaisesRegex(RuntimeError, "start failed"):
            with bootstrap.Database(self.work, bootstrap.INIT):
                self.fail("failed start must not enter the test")
        self.assertEqual(daemon.containers, {})

    def test_cleanup_ignores_name_reuse_and_other_runs(self):
        daemon = Docker()
        with patch.object(bootstrap, "docker", daemon):
            with bootstrap.Database(self.work, bootstrap.INIT) as database:
                # Rename the owned container and reuse its old name. Add another
                # bootstrap run with the same label key and a different token.
                daemon.containers[daemon.owned_id]["name"] = "renamed-owned-container"
                daemon.foreign(database.name)
                labels = daemon.containers[daemon.owned_id]["labels"]
                daemon.containers["c" * 64] = {"name": "other-bootstrap-run",
                                               "labels": {key: value + "-other" for key, value in labels.items()}}
        self.assertEqual(set(daemon.containers), {daemon.foreign_id, "c" * 64})


if __name__ == "__main__":
    unittest.main()
