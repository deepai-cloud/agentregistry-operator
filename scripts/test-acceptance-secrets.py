#!/usr/bin/env python3
"""Check acceptance failure-log redaction with synthetic credentials; no cluster access."""

import base64
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "managed_acceptance", Path(__file__).with_name("test-managed-postgres.py"))
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)


class AcceptanceSecretsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="areg-secret-log-test-")
        self.addCleanup(temporary.cleanup)
        with patch.object(acceptance, "ROOT", Path(temporary.name)):
            self.runner = acceptance.Acceptance()
        self.values = ["synthetic-auth-password", "b" * 64, "synthetic-database-password",
                       "postgres://registry:synthetic-database-password@database:5432/registry",
                       "synthetic-administrator-password"]
        self.runner.sensitive.extend(self.values)
        self.encoded = [base64.b64encode(value.encode()).decode() for value in self.values]

    def assert_redacted(self, text):
        for value in self.values + self.encoded:
            self.assertNotIn(value, text, "credential survived failure-log redaction")
        self.assertIn("[redacted]", text)

    def test_raw_and_encoded_credentials_in_patch_diagnostics_are_redacted(self):
        original = json.dumps({"kind": "Secret", "data": dict(enumerate(self.encoded))})
        modified = json.dumps({"kind": "Secret", "stringData": dict(enumerate(self.values))})
        message = f"creating patch with:\noriginal:\n{original}\nmodified:\n{modified}\nfor: invalid patch"
        safe = self.runner.sanitize(message)
        self.assert_redacted(safe)
        self.assertIn("creating patch with:", safe)
        self.assertIn("for: invalid patch", safe)
        self.assertNotIn("postgres://", safe, "a password substring hid the complete URL from redaction")
        self.assertEqual(self.runner.sanitize(safe), safe)

    def test_failed_command_sanitizes_both_output_streams(self):
        result = subprocess.CompletedProcess(["kubectl"], 1, "\n".join(self.values), "\n".join(self.encoded))
        with patch.object(acceptance.subprocess, "run", return_value=result), self.assertRaises(RuntimeError) as failure:
            self.runner.kubectl("apply", "-f", "-", input="synthetic secret manifest")
        self.assert_redacted(str(failure.exception))
        self.assertIn("Command failed: kubectl", str(failure.exception))

    def test_successful_secret_reads_remain_available_to_the_test(self):
        payload = json.dumps({"data": dict(enumerate(self.encoded))})
        result = subprocess.CompletedProcess(["kubectl"], 0, payload, "")
        with patch.object(acceptance.subprocess, "run", return_value=result):
            self.assertEqual(self.runner.get("secret", "database")["data"], json.loads(payload)["data"])

    def test_diagnostics_sanitize_unchecked_command_output(self):
        self.runner.kubeconfig.write_text("synthetic kubeconfig")
        result = subprocess.CompletedProcess(["kubectl"], 1, "\n".join(self.values + self.encoded), "")
        with patch.object(acceptance.subprocess, "run", return_value=result), patch.object(acceptance, "say") as say:
            self.runner.diagnostics()
        self.assertEqual(say.call_count, 3)
        for call in say.call_args_list:
            self.assert_redacted(call.args[0])


if __name__ == "__main__":
    unittest.main()
