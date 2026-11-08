#!/usr/bin/env python3
"""Check acceptance cleanup failure paths without Docker or Kubernetes."""

import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch


spec = importlib.util.spec_from_file_location(
    "managed_acceptance", Path(__file__).with_name("test-managed-postgres.py"))
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)


class AcceptanceCleanupTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="areg-acceptance-cleanup-")
        self.addCleanup(temporary.cleanup)
        for replacement in (patch.object(acceptance, "ROOT", Path(temporary.name)),
                            patch.object(acceptance, "say")):
            replacement.start()
            self.addCleanup(replacement.stop)
        self.runner = acceptance.Acceptance()
        self.runner.kubeconfig.write_text("synthetic owned cluster credentials")
        self.runner.created = True
        self.runner.run = Mock()
        self.runner.forward = Mock()
        self.runner.forward_output = Mock()

    def assert_cluster_removed(self):
        self.runner.run.assert_called_once_with(
            [self.runner.k3d, "cluster", "delete", self.runner.cluster], timeout=150)
        self.assertFalse(self.runner.kubeconfig.exists())

    def test_normal_cleanup_stops_forward_and_removes_owned_cluster(self):
        self.runner.cleanup()
        self.runner.forward.terminate.assert_called_once_with()
        self.runner.forward.wait.assert_called_once_with(timeout=10)
        self.runner.forward.kill.assert_not_called()
        self.runner.forward_output.close.assert_called_once_with()
        self.assert_cluster_removed()

    def test_unresponsive_forward_is_killed_before_cluster_cleanup(self):
        self.runner.forward.wait.side_effect = [subprocess.TimeoutExpired("kubectl", 10), 0]
        self.runner.cleanup()
        self.runner.forward.kill.assert_called_once_with()
        self.assertEqual(self.runner.forward.wait.call_count, 2)
        self.assert_cluster_removed()

    def test_forward_errors_do_not_skip_log_or_cluster_cleanup(self):
        for stage in ("terminate", "kill", "wait"):
            with self.subTest(stage=stage):
                self.runner.run.reset_mock()
                self.runner.kubeconfig.write_text("synthetic owned cluster credentials")
                self.runner.forward = Mock()
                self.runner.forward_output = Mock()
                if stage != "terminate":
                    self.runner.forward.wait.side_effect = subprocess.TimeoutExpired("kubectl", 10)
                if stage != "wait":
                    getattr(self.runner.forward, stage).side_effect = OSError("synthetic process failure")
                with self.assertRaises(Exception) as caught:
                    self.runner.cleanup()
                self.runner.forward_output.close.assert_called_once_with()
                self.assert_cluster_removed()
                self.assertIn("port-forward", str(caught.exception))

    def test_log_close_failure_does_not_skip_cluster_cleanup(self):
        self.runner.forward_output.close.side_effect = OSError("synthetic log failure")
        with self.assertRaisesRegex(Exception, "synthetic log failure"):
            self.runner.cleanup()
        self.assert_cluster_removed()

    def test_multiple_cleanup_failures_are_reported_and_keep_recovery_config(self):
        self.runner.forward.terminate.side_effect = OSError("synthetic process failure")
        self.runner.forward_output.close.side_effect = OSError("synthetic log failure")
        self.runner.run.side_effect = RuntimeError("synthetic cluster failure")
        with self.assertRaises(Exception) as caught:
            self.runner.cleanup()
        for message in ("synthetic process failure", "synthetic log failure", "synthetic cluster failure"):
            self.assertIn(message, str(caught.exception))
        self.runner.forward_output.close.assert_called_once_with()
        self.runner.run.assert_called_once_with(
            [self.runner.k3d, "cluster", "delete", self.runner.cluster], timeout=150)
        self.assertTrue(self.runner.kubeconfig.exists(), "failed deletion must retain recovery credentials")

    def test_failure_before_creation_never_deletes_a_cluster(self):
        self.runner.created = False
        self.runner.forward = None
        self.runner.forward_output = None
        self.runner.cleanup()
        self.runner.run.assert_not_called()
        self.assertFalse(self.runner.kubeconfig.exists())

    def test_cleanup_error_fails_result_and_redacts_report_after_successful_checks(self):
        # Run main's real error/evidence path with cluster operations replaced.
        for method in ("prerequisites", "create_cluster", "install", "create_registry",
                       "inspect_admission", "inspect_database", "inspect_http",
                       "restart_database", "inspect_cleanup", "inspect_recovery"):
            setattr(self.runner, method, Mock())
        password = "synthetic-sensitive-password"
        self.runner.sensitive = [password]
        self.runner.forward_output.close.side_effect = OSError("log failed: " + password)
        with patch.object(acceptance, "Acceptance", return_value=self.runner), \
                patch.object(acceptance.os, "umask"), patch.object(acceptance, "say") as output:
            self.assertEqual(acceptance.main(), 1)
        self.assert_cluster_removed()
        result = json.loads((self.runner.work / "result.json").read_text())
        self.assertIs(result["passed"], False)
        report = "\n".join(call.args[0] for call in output.call_args_list)
        self.assertIn("Cleanup failed:", report)
        self.assertIn("log failed: [redacted]", report)
        self.assertNotIn(password, report)


if __name__ == "__main__":
    unittest.main()
