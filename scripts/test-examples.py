#!/usr/bin/env python3
"""Reconcile database examples with generated test Secrets; no cluster or Docker."""

import base64
import copy
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[1]


def run(*args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, text=True, capture_output=True,
                          timeout=120, **kwargs).stdout


class DatabaseExamplesTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        temporary = tempfile.TemporaryDirectory(prefix="areg-examples-")
        cls.addClassCleanup(temporary.cleanup)
        work = Path(temporary.name)
        # Helm is already required by make verify. Use its YAML parser instead
        # of adding a Python/Go dependency or interpreting YAML with regexes.
        chart = work / "chart"
        (chart / "templates").mkdir(parents=True)
        (chart / "fixtures").mkdir()
        (chart / "Chart.yaml").write_text("apiVersion: v2\nname: example-check\nversion: 0.0.0\n")
        (chart / "templates/examples.yaml").write_text(
            '{{- $documents := dict -}}\n'
            '{{- range $path, $_ := .Files.Glob "fixtures/*" -}}\n'
            '{{- $_ := set $documents (base $path) ($.Files.Get $path | fromYaml) -}}\n'
            '{{- end -}}\n{{ $documents | toJson }}\n')
        for name in ("private", "managed-registry", "managed-storage", "external-database"):
            (chart / "fixtures" / (name + ".yaml")).write_text((ROOT / "examples" / (name + ".yaml")).read_text())
        for index, document in enumerate(re.split(r"(?m)^---\s*$", (ROOT / "examples/local/postgres.yaml").read_text())):
            (chart / "fixtures" / f"postgres-{index}.yaml").write_text(document)
        rendered = run(os.environ.get("HELM", "helm"), "template", "examples", str(chart))
        cls.documents = json.loads("\n".join(line for line in rendered.splitlines()
                                             if not line.startswith(("#", "---"))))
        cls.helper = work / "example-sync"
        run(os.environ.get("GO", "go"), "build", "-o", str(cls.helper), "./internal/operator/testdata/example-sync")
        output = work / "credentials"
        run(sys.executable, "examples/local/create-secrets.py", "--external-database", "--output-dir", str(output))
        cls.secrets = {}
        for secret in json.loads((output / "secrets.json").read_text())["items"]:
            secret["data"] = {key: base64.b64encode(value.encode()).decode()
                              for key, value in secret.pop("stringData").items()}
            cls.secrets[secret["metadata"]["name"]] = secret

    def request(self, name):
        parent = copy.deepcopy(self.documents[name + ".yaml"])
        parent["metadata"].update(uid="example-check", generation=1)
        return {"parent": parent, "related": {"Secret.v1": self.secrets}}

    def sync(self, request):
        return json.loads(run(str(self.helper), input=json.dumps(request)))

    def check_running_application(self, response):
        conditions = {condition["type"]: condition["status"] for condition in response["status"]["conditions"]}
        self.assertEqual(conditions["DependenciesReady"], "True")
        deployment = next(child for child in response["children"] if child["kind"] == "Deployment")
        self.assertEqual(deployment["spec"]["replicas"], 1)
        return deployment["spec"]["template"]["spec"]

    def test_external_example_uses_supplied_database_without_provisioning_one(self):
        request = self.request("external-database")
        response = self.sync(request)
        self.assertCountEqual([child["kind"] for child in response["children"]],
                              ["ServiceAccount", "NetworkPolicy", "Service", "Deployment"],
                              "external example must not create managed database resources")
        database = request["parent"]["spec"]["database"]
        secret = self.secrets[database["secretName"]]
        address = urlsplit(base64.b64decode(secret["data"]["url"]).decode())
        self.assertEqual(database["host"], address.hostname)
        namespace = request["parent"]["metadata"]["namespace"]
        postgres = [document for name, document in self.documents.items() if name.startswith("postgres-")]
        service = next(document for document in postgres if document["kind"] == "Service")
        self.assertEqual(database["host"], f"{service['metadata']['name']}.{namespace}.svc.cluster.local")
        self.assertEqual(service["metadata"]["namespace"], namespace)
        database_pod = next(document for document in postgres if document["kind"] == "Deployment")["spec"]["template"]
        policy = next(child for child in response["children"] if child["kind"] == "NetworkPolicy")
        peer = policy["spec"]["egress"][0]["to"][0]["podSelector"]["matchLabels"]
        for selector in (service["spec"]["selector"], peer):
            self.assertTrue(selector.items() <= database_pod["metadata"]["labels"].items(),
                            "Service and registry egress must select the example database pods")
        pod = self.check_running_application(response)
        registry = next(container for container in pod["containers"] if container["name"] == "registry")
        connection = next(env for env in registry["env"] if env["name"] == "AGENT_REGISTRY_DATABASE_URL")
        self.assertEqual(connection["valueFrom"]["secretKeyRef"], {"name": database["secretName"], "key": "url"})
        volume = next(volume for volume in pod["volumes"] if volume["name"] == "database")
        self.assertEqual(volume["secret"]["secretName"], database["secretName"])

    def test_managed_examples_provision_their_documented_storage(self):
        for name, storage in (("private", "1Gi"), ("managed-registry", "1Gi"), ("managed-storage", "10Gi")):
            with self.subTest(example=name):
                request = self.request(name)
                initial = self.sync(request)
                self.assertFalse(any(child["kind"] == "StatefulSet" for child in initial["children"]),
                                 "managed examples must persist generated credentials before provisioning storage")
                request["children"] = {"observed": {str(index): child for index, child in enumerate(initial["children"])}}
                response = self.sync(request)
                self.check_running_application(response)
                self.assertEqual(len(response["children"]), 9)
                database = next(child for child in response["children"] if child["kind"] == "StatefulSet")
                self.assertEqual(database["spec"]["replicas"], 1)
                claim = database["spec"]["volumeClaimTemplates"][0]["spec"]
                self.assertEqual(claim["resources"]["requests"]["storage"], storage)


if __name__ == "__main__":
    unittest.main()
