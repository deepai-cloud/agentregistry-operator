#!/usr/bin/env python3
"""Check publishing event boundaries and version uniqueness without credentials."""
from pathlib import Path
import runpy
import unittest

coordinates = runpy.run_path(str(Path(__file__).with_name("release-coordinates.py")))["coordinates"]


class ReleaseCoordinatesTest(unittest.TestCase):
    def env(self, **overrides):
        return {
            "GITHUB_REPOSITORY": "DeepAI-Cloud/AgentRegistry-Operator",
            "GITHUB_SHA": "abcdef0123456789" * 2 + "abcdef01",
            "GITHUB_EVENT_NAME": "push",
            "GITHUB_REF": "refs/heads/main",
            "GITHUB_RUN_ID": "12345",
            "GITHUB_RUN_ATTEMPT": "1",
            **overrides,
        }

    def test_main_snapshot_versions_match_and_reruns_are_unique(self):
        first = coordinates(self.env(), "0.1.0")
        self.assertEqual(first["version"], "0.1.0-dev.12345.1.gabcdef012345")
        self.assertEqual(first["image"], "ghcr.io/deepai-cloud/agentregistry-operator:v" + first["version"])
        self.assertEqual(first["release_tag"], "dev-12345-1-abcdef012345")
        self.assertEqual(first["chart_registry"], "oci://ghcr.io/deepai-cloud/charts")
        self.assertEqual((first["prerelease"], first["snapshot"]), ("true", "true"))
        second = coordinates(self.env(GITHUB_RUN_ATTEMPT="2"), "0.1.0")
        another = coordinates(self.env(GITHUB_RUN_ID="12346"), "0.1.0")
        for result in (second, another):
            self.assertNotEqual(first["version"], result["version"])
            self.assertNotEqual(first["release_tag"], result["release_tag"])

    def test_manual_run_and_prerelease_chart(self):
        result = coordinates(self.env(GITHUB_EVENT_NAME="workflow_dispatch"), "1.2.3-rc.1")
        self.assertEqual(result["version"], "1.2.3-dev.12345.1.gabcdef012345")

    def test_stable_and_prerelease_tags(self):
        for version, prerelease in (("1.2.3", "false"), ("1.2.3-rc.1", "true")):
            with self.subTest(version=version):
                result = coordinates(self.env(GITHUB_REF="refs/tags/v" + version), "0.1.0")
                self.assertEqual(result["version"], version)
                self.assertEqual(result["release_tag"], "v" + version)
                self.assertEqual(result["image"].rsplit(":", 1)[1], "v" + version)
                self.assertEqual((result["prerelease"], result["snapshot"]), (prerelease, "false"))

    def test_other_events_and_refs_cannot_publish(self):
        for event, ref in (
            ("pull_request", "refs/heads/main"),
            ("pull_request_target", "refs/heads/main"),
            ("push", "refs/heads/feature"),
            ("workflow_dispatch", "refs/heads/feature"),
            ("workflow_dispatch", "refs/tags/v1.2.3"),
            ("push", "refs/tags/dev-12345-1-abcdef012345"),
        ):
            with self.subTest(event=event, ref=ref), self.assertRaises(ValueError):
                coordinates(self.env(GITHUB_EVENT_NAME=event, GITHUB_REF=ref), "0.1.0")

    def test_invalid_versions_and_output_injection_fail(self):
        for tag in ("v01.2.3", "v1.2", "v1.2.3+build", "v1.2.3-01", "v1.2.3\nimage=evil"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                coordinates(self.env(GITHUB_REF="refs/tags/" + tag), "0.1.0")
        for override in (
            {"GITHUB_REPOSITORY": "owner/repo\nimage=evil"},
            {"GITHUB_SHA": "abcdef012345"},
            {"GITHUB_RUN_ID": "0"},
            {"GITHUB_RUN_ATTEMPT": "01"},
        ):
            with self.subTest(override=override), self.assertRaises(ValueError):
                coordinates(self.env(**override), "0.1.0")


if __name__ == "__main__":
    unittest.main()
