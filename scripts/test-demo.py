#!/usr/bin/env python3
"""Exercise the demo command flow with fake CLIs; never contact Docker or Kubernetes."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
CRD = "charts/agentregistry-operator/crds/agentregistries.json"
MOCK = r'''#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import sys

tool = Path(sys.argv[0]).name
args = sys.argv[1:]
state = Path(os.environ["DEMO_TEST_STATE"])
entry = {"tool": tool, "args": args}
if tool == "kubectl" and args[-3:] == ["apply", "-f", os.environ["DEMO_TEST_CRD"]]:
    entry["schema"] = hashlib.sha256(Path(args[-1]).read_bytes()).hexdigest()
with (state / "calls.jsonl").open("a") as log:
    log.write(json.dumps(entry) + "\n")
if tool == "docker":
    if args[0] == "inspect":
        node = state / "node-id"
        if not node.exists():
            sys.exit(1)
        if ".Config.Labels" in " ".join(args):
            owner = state / "node-owner"
            print(owner.read_text().strip() if owner.exists() else "")
        else:
            print(node.read_text().strip())
    elif args[:2] == ["image", "inspect"]:
        print("sha256:" + "a" * 64)
elif tool == "k3d":
    if args[:2] == ["cluster", "create"]:
        (state / "node-id").write_text("owned-node\n")
        if "--runtime-label" in args:
            label = args[args.index("--runtime-label") + 1]
            assert label.startswith("registry.deepai.cloud/demo-owner=") and label.endswith("@server:0")
            token = label.split("=", 1)[1].removesuffix("@server:0")
            # The token must be durable before asking Docker to create a node.
            assert (state / "creation-token").read_text().strip() == token
            if os.environ.get("DEMO_TEST_FAIL") != "create-unlabelled":
                if os.environ.get("DEMO_TEST_FAIL") == "create-wrong-owner":
                    token = "f" * 64 if token != "f" * 64 else "e" * 64
                (state / "node-owner").write_text(token)
        if os.environ.get("DEMO_TEST_FAIL") == "create":
            sys.exit(1)
    elif args[:2] == ["cluster", "delete"]:
        (state / "node-id").unlink()
        (state / "node-owner").unlink(missing_ok=True)
    elif args[:2] == ["kubeconfig", "get"]:
        failure = os.environ.get("DEMO_TEST_FAIL")
        if failure == "partial-config":
            print("partial-kubeconfig")
            sys.exit(1)
        if failure == "empty-config":
            sys.exit(0)
        if failure == "wrong-context":
            print("another-kubeconfig")
            sys.exit(0)
        print("fake-demo-kubeconfig")
elif tool == "kubectl":
    if "config" in args:
        assert args[0] == "--kubeconfig" and args[2:] == ["config", "view", "--minify", "-o", "jsonpath={.current-context}"]
        config = Path(args[1]).read_text().strip()
        if config == "fake-demo-kubeconfig":
            print("k3d-agentregistry-demo")
        elif config == "another-kubeconfig":
            print("k3d-other")
        else:
            sys.exit(1)
    failure = os.environ.get("DEMO_TEST_FAIL", "")
    if failure == "apply" and "schema" in entry:
        sys.exit(1)
    if failure == "established" and "crd/agentregistries.registry.deepai.cloud" in args:
        sys.exit(1)
'''


class DemoTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="areg-demo-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        (self.root / "scripts").mkdir()
        shutil.copy2(ROOT / "scripts/demo.sh", self.root / "scripts/demo.sh")
        self.crd = self.root / CRD
        self.crd.parent.mkdir(parents=True)
        shutil.copy2(ROOT / CRD, self.crd)
        self.state = self.root / ".local/quickstart"
        self.state.mkdir(parents=True)
        # Reuse credentials; no real secrets need to be generated for command tests.
        (self.state / "secrets.json").write_text("{}")
        binary = self.root / "fake-bin"
        binary.mkdir()
        for name in ("docker", "k3d", "kubectl", "helm"):
            executable = binary / name
            executable.write_text(MOCK)
            executable.chmod(0o755)
        self.env = {**os.environ, "PATH": str(binary) + os.pathsep + os.environ["PATH"],
                    "K3D": str(binary / "k3d"), "HELM": str(binary / "helm"),
                    "DEMO_TEST_STATE": str(self.state), "DEMO_TEST_CRD": CRD,
                    "DEMO_TEST_FAIL": ""}

    def run_demo(self, action="up", **overrides):
        return subprocess.run(["bash", "scripts/demo.sh", action], cwd=self.root,
                              env={**self.env, **overrides}, text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)

    def calls(self):
        return [json.loads(line) for line in (self.state / "calls.jsonl").read_text().splitlines()]

    def own_existing_cluster(self):
        (self.state / "node-id").write_text("owned-node\n")
        (self.state / "owned-node-id").write_text("owned-node\n")

    def assert_upgrade_sequence(self, calls):
        applied = [i for i, call in enumerate(calls) if "schema" in call]
        established = [i for i, call in enumerate(calls)
                       if call["tool"] == "kubectl" and "crd/agentregistries.registry.deepai.cloud" in call["args"]]
        upgraded = [i for i, call in enumerate(calls) if call["tool"] == "helm"]
        self.assertEqual((len(applied), len(established), len(upgraded)), (1, 1, 1))
        self.assertLess(applied[0], established[0])
        self.assertLess(established[0], upgraded[0])
        self.assertIn("--for=condition=Established", calls[established[0]]["args"])
        for call in calls:
            if call["tool"] == "kubectl" and "config" not in call["args"]:
                self.assertEqual(call["args"][:4], ["--kubeconfig", str(self.state / "kubeconfig"),
                                                     "--context", "k3d-agentregistry-demo"])
        return calls[applied[0]]["schema"]

    def test_fresh_install_and_rerun_apply_current_schema_before_helm(self):
        first = self.run_demo()
        self.assertEqual(first.returncode, 0, first.stderr)
        first_calls = self.calls()
        first_schema = self.assert_upgrade_sequence(first_calls)
        creation = [call for call in first_calls if call["tool"] == "k3d" and call["args"][:2] == ["cluster", "create"]]
        self.assertEqual(len(creation), 1)
        self.assertIn("--kubeconfig-update-default=false", creation[0]["args"])
        self.assertIn("--kubeconfig-switch-context=false", creation[0]["args"])
        self.assertEqual((self.state / "owned-node-id").read_text().strip(), "owned-node")
        kubeconfig = self.state / "kubeconfig"
        self.assertEqual(kubeconfig.read_text(), "fake-demo-kubeconfig\n")
        self.assertEqual(kubeconfig.stat().st_mode & 0o777, 0o600)
        # A changed checkout must refresh the schema on an already-owned cluster.
        schema = json.loads(self.crd.read_text())
        schema["metadata"]["annotations"] = {"test-schema-revision": "2"}
        self.crd.write_text(json.dumps(schema))
        second = self.run_demo()
        self.assertEqual(second.returncode, 0, second.stderr)
        rerun_calls = self.calls()[len(first_calls):]
        self.assertNotEqual(first_schema, self.assert_upgrade_sequence(rerun_calls))
        self.assertFalse(any(call["tool"] == "k3d" and call["args"][:2] == ["cluster", "create"] for call in rerun_calls))

    def test_failed_kubeconfig_refresh_preserves_access(self):
        self.own_existing_cluster()
        kubeconfig = self.state / "kubeconfig"
        for failure in ("partial-config", "empty-config", "wrong-context"):
            with self.subTest(failure=failure):
                kubeconfig.write_text("previous-working-config\n")
                (self.state / "calls.jsonl").write_text("")
                result = self.run_demo(DEMO_TEST_FAIL=failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(kubeconfig.read_text(), "previous-working-config\n")
                self.assertEqual(list(self.state.glob("kubeconfig.*")), [])
                self.assertFalse(any(call["tool"] == "helm" or
                                     call["tool"] == "docker" and call["args"][0] == "build" or
                                     call["tool"] == "kubectl" and "config" not in call["args"]
                                     for call in self.calls()))

    def test_crd_failure_stops_before_installing_controller_or_registry(self):
        self.own_existing_cluster()
        for failure in ("apply", "established"):
            with self.subTest(failure=failure):
                (self.state / "calls.jsonl").write_text("")
                result = self.run_demo(DEMO_TEST_FAIL=failure)
                self.assertNotEqual(result.returncode, 0)
                calls = self.calls()
                self.assertFalse(any(call["tool"] == "helm" for call in calls))
                self.assertFalse(any("examples/private.yaml" in call["args"] or str(self.state / "secrets.json") in call["args"] for call in calls))

    def test_unowned_cluster_is_rejected_before_any_mutation(self):
        self.own_existing_cluster()
        (self.state / "node-id").write_text("another-checkouts-node\n")
        for action in ("up", "down"):
            with self.subTest(action=action):
                (self.state / "calls.jsonl").write_text("")
                result = self.run_demo(action)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("not owned by this checkout", result.stderr)
                self.assertTrue(all(call["tool"] == "docker" and call["args"][0] in ("version", "inspect") for call in self.calls()))

    def test_cleanup_does_not_apply_a_schema(self):
        self.own_existing_cluster()
        result = self.run_demo("down")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(call["tool"] == "k3d" and call["args"] == ["cluster", "delete", "agentregistry-demo"] for call in self.calls()))
        self.assertFalse(any(call["tool"] in ("helm", "kubectl") for call in self.calls()))
        self.assertTrue((self.state / "secrets.json").exists())

    def test_failed_creation_can_be_cleaned_up_without_a_saved_node_id(self):
        result = self.run_demo(DEMO_TEST_FAIL="create")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.state / "node-id").exists())
        self.assertFalse((self.state / "owned-node-id").exists())
        before_cleanup = len(self.calls())
        result = self.run_demo("down")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(call["tool"] == "k3d" and call["args"] == ["cluster", "delete", "agentregistry-demo"]
                            for call in self.calls()[before_cleanup:]))
        self.assertFalse((self.state / "node-id").exists())
        self.assertFalse((self.state / "creation-token").exists())
        self.assertTrue((self.state / "secrets.json").exists())

    def test_matching_creation_token_recovers_missing_or_stale_node_id(self):
        for saved_id in (None, "previous-cluster-node"):
            with self.subTest(saved_id=saved_id):
                (self.state / "calls.jsonl").write_text("")
                (self.state / "node-id").write_text("owned-node")
                (self.state / "creation-token").write_text("a" * 64 + "\n")
                (self.state / "node-owner").write_text("a" * 64)
                saved = self.state / "owned-node-id"
                if saved_id is None:
                    saved.unlink(missing_ok=True)
                else:
                    saved.write_text(saved_id)
                result = self.run_demo()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(saved.read_text().strip(), "owned-node")
                self.assertFalse(any(call["tool"] == "k3d" and call["args"][:2] == ["cluster", "create"]
                                     for call in self.calls()))
                self.assert_upgrade_sequence(self.calls())

    def test_creation_token_must_match_before_recovering_a_node(self):
        for token, label in (("a" * 64, "b" * 64), ("a" * 64, None),
                             (None, "a" * 64), ("", ""), ("<no value>", "<no value>")):
            for action in ("up", "down"):
                with self.subTest(token=token, label=label, action=action):
                    (self.state / "calls.jsonl").write_text("")
                    (self.state / "node-id").write_text("foreign-node")
                    for name, value in (("creation-token", token), ("node-owner", label)):
                        path = self.state / name
                        if value is None:
                            path.unlink(missing_ok=True)
                        else:
                            path.write_text(value)
                    result = self.run_demo(action)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("not owned by this checkout", result.stderr)
                    self.assertFalse((self.state / "owned-node-id").exists())
                    self.assertTrue(all(call["tool"] == "docker" and call["args"][0] in ("version", "inspect")
                                        for call in self.calls()))

    def test_successful_creation_still_requires_the_expected_owner_label(self):
        for failure in ("create-unlabelled", "create-wrong-owner"):
            with self.subTest(failure=failure):
                (self.state / "node-id").unlink(missing_ok=True)
                (self.state / "node-owner").unlink(missing_ok=True)
                (self.state / "owned-node-id").unlink(missing_ok=True)
                (self.state / "calls.jsonl").write_text("")
                result = self.run_demo(DEMO_TEST_FAIL=failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Cannot verify newly created demo cluster ownership", result.stderr)
                self.assertFalse((self.state / "owned-node-id").exists())
                self.assertTrue(all(call["tool"] == "docker" and call["args"][0] in ("version", "inspect")
                                    or call["tool"] == "k3d" and call["args"][:2] == ["cluster", "create"]
                                    for call in self.calls()))


if __name__ == "__main__":
    unittest.main()
