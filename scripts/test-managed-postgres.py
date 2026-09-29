#!/usr/bin/env python3
"""Exercise the bundled install and managed database in an owned, disposable K3s cluster.

Requires Docker, k3d 5.9.0, kubectl, Helm, and Go. All Kubernetes operations use a
private kubeconfig and explicit context. No existing cluster or default context
is selected, modified, or deleted. Secrets are sent on stdin and never logged.
"""

import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[1]
K3S_IMAGE = "rancher/k3s:v1.34.11-k3s1@sha256:5d52389a0f4fd7ebdb5a1fb2d7c67c35da966230782c4abb0667d86bcccea9c2"
NAMESPACE = "managed-acceptance"
SYSTEM_NAMESPACE = "agentregistry-system"
NAME = "catalog"
DB_NAME = NAME + "-postgres"


def say(message):
    print(message, flush=True)


class Acceptance:
    def __init__(self):
        self.cluster = "areg-managed-" + uuid.uuid4().hex[:10]
        self.context = "k3d-" + self.cluster
        self.work = ROOT / ".cache" / self.cluster
        self.work.mkdir(parents=True, mode=0o700)
        self.kubeconfig = self.work / "kubeconfig"
        self.env = os.environ.copy()
        self.env["KUBECONFIG"] = str(self.kubeconfig)
        self.env["HELM_CACHE_HOME"] = str(self.work / "helm" / "cache")
        self.env["HELM_CONFIG_HOME"] = str(self.work / "helm" / "config")
        self.env["HELM_DATA_HOME"] = str(self.work / "helm" / "data")
        self.k3d = os.environ.get("K3D") or shutil.which("k3d") or str(ROOT / ".cache/tools/k3d")
        self.sensitive = []
        self.created = False
        self.forward = None
        self.forward_output = None
        self.evidence = {"cluster": self.cluster, "k3s_image": K3S_IMAGE, "checks": []}

    def sanitize(self, text):
        for value in self.sensitive:
            text = text.replace(value, "[redacted]")
        return text

    def run(self, command, *, input=None, timeout=120, check=True):
        result = subprocess.run(
            [str(part) for part in command], input=input, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=self.env,
            cwd=ROOT, timeout=timeout,
        )
        if check and result.returncode:
            raise RuntimeError(self.sanitize(
                f"Command failed: {' '.join(str(x) for x in command)}\n{result.stdout}\n{result.stderr}"
            ))
        return result

    def kubectl(self, *args, **kwargs):
        return self.run(["kubectl", "--kubeconfig", self.kubeconfig, "--context", self.context, *args], **kwargs)

    def get(self, resource, name=None, namespace=NAMESPACE):
        args = ["get", resource]
        if name:
            args.append(name)
        return json.loads(self.kubectl(*args, "-n", namespace, "-o", "json").stdout)

    def apply(self, obj):
        self.kubectl("apply", "-f", "-", input=json.dumps(obj))

    def check(self, message):
        self.evidence["checks"].append(message)
        say("PASS " + message)

    def wait(self, message, predicate, timeout=240):
        deadline = time.monotonic() + timeout
        last_progress = 0
        while time.monotonic() < deadline:
            if predicate():
                return
            if time.monotonic() - last_progress > 25:
                say("Waiting: " + message)
                last_progress = time.monotonic()
            time.sleep(2)
        raise RuntimeError("Timed out: " + message)

    def ready(self):
        obj = self.get("agentregistry", NAME)
        return any(c["type"] == "Ready" and c["status"] == "True"
                   and c.get("observedGeneration") == obj["metadata"]["generation"]
                   for c in obj.get("status", {}).get("conditions", []))

    def prerequisites(self):
        for tool in ["docker", "kubectl", "helm", "go"]:
            if not shutil.which(tool):
                raise RuntimeError("Missing prerequisite: " + tool)
        if not Path(self.k3d).is_file():
            raise RuntimeError("Install k3d 5.9.0 or set K3D to its executable path")
        version = self.run([self.k3d, "version"]).stdout
        if "k3d version v5.9.0" not in version:
            raise RuntimeError("This acceptance runner requires the tested k3d version 5.9.0")
        # Respect an explicit DOCKER_CONTEXT. Otherwise use Docker's current
        # context; select desktop-linux only when the current daemon is stopped.
        docker = self.run(["docker", "version", "--format", "{{.Server.Version}}"], check=False)
        if docker.returncode and "DOCKER_CONTEXT" not in self.env:
            probe = self.run(["docker", "--context", "desktop-linux", "version", "--format", "{{.Server.Version}}"], check=False)
            if probe.returncode == 0:
                self.env["DOCKER_CONTEXT"] = "desktop-linux"
                docker = probe
        if docker.returncode:
            raise RuntimeError("A running Docker daemon is required; set DOCKER_CONTEXT if needed")
        self.evidence["docker_version"] = docker.stdout.strip()
        self.evidence["k3d_version"] = version.strip().splitlines()[0]
        self.evidence["helm_version"] = self.run(["helm", "version", "--short"]).stdout.strip()

    def create_cluster(self):
        say("Creating isolated cluster " + self.cluster)
        # Set ownership before creation so a failed partial creation is removed.
        self.created = True
        self.run([
            self.k3d, "cluster", "create", self.cluster, "--image", K3S_IMAGE,
            "--servers", "1", "--agents", "0", "--no-lb",
            "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false",
            "--k3s-arg", "--disable=traefik@server:*", "--wait", "--timeout", "180s",
        ], timeout=240)
        self.kubeconfig.write_text(self.run([self.k3d, "kubeconfig", "get", self.cluster]).stdout)
        self.kubeconfig.chmod(0o600)
        current = self.kubectl("config", "current-context").stdout.strip()
        if current != self.context:
            raise RuntimeError("Unexpected context in isolated kubeconfig")
        self.kubectl("wait", "node", "--all", "--for=condition=Ready", "--timeout=180s", timeout=190)
        self.evidence["kubernetes_version"] = json.loads(self.kubectl("version", "-o", "json").stdout)["serverVersion"]["gitVersion"]

    def install(self):
        image = "agentregistry-operator:" + self.cluster
        source = hashlib.sha256()
        paths = [ROOT / "Dockerfile", ROOT / "go.mod"]
        paths.extend(path for folder in ("cmd", "internal", "charts") for path in (ROOT / folder).rglob("*") if path.is_file())
        for path in sorted(paths):
            source.update(str(path.relative_to(ROOT)).encode() + b"\0" + path.read_bytes() + b"\0")
        self.evidence["source_sha256"] = source.hexdigest()
        say("Building and importing current operator source")
        self.run(["docker", "build", "-t", image, "."], timeout=600)
        self.run([self.k3d, "image", "import", image, "--cluster", self.cluster], timeout=180)
        say("Installing operator and bundled Metacontroller with one Helm install")
        self.run([
            "helm", "install", "agentregistry", "./charts/agentregistry-operator",
            "--kubeconfig", self.kubeconfig, "--kube-context", self.context,
            "--namespace", SYSTEM_NAMESPACE, "--create-namespace",
            "--set", "image.repository=agentregistry-operator",
            "--set", "image.tag=" + self.cluster, "--wait", "--timeout=240s",
        ], timeout=270)
        releases = json.loads(self.run([
            "helm", "list", "--all-namespaces", "--kubeconfig", self.kubeconfig,
            "--kube-context", self.context, "--output", "json",
        ]).stdout)
        if len(releases) != 1:
            raise RuntimeError("Expected exactly one Helm release")
        system = self.get("pods", namespace=SYSTEM_NAMESPACE)
        self.evidence["system_images"] = sorted({c["image"] for p in system["items"] for c in p["spec"]["containers"]})
        self.evidence["system_image_ids"] = sorted({c["imageID"] for p in system["items"] for c in p.get("status", {}).get("containerStatuses", [])})
        self.check("one Helm release installs a ready operator and bundled Metacontroller")

    def create_registry(self):
        self.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": NAMESPACE}})
        self.apply({
            "apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": "default-deny-ingress", "namespace": NAMESPACE},
            "spec": {"podSelector": {}, "policyTypes": ["Ingress"], "ingress": []},
        })
        self.username, self.password = "acceptance", secrets.token_hex(24)
        jwt = secrets.token_hex(32)
        self.sensitive.extend([self.password, jwt])
        self.apply({
            "apiVersion": "v1", "kind": "Secret", "metadata": {"name": "registry-auth", "namespace": NAMESPACE},
            "stringData": {"username": self.username, "password": self.password, "jwt-key": jwt},
        })
        self.apply({
            "apiVersion": "registry.deepai.cloud/v1alpha1", "kind": "AgentRegistry",
            "metadata": {"name": NAME, "namespace": NAMESPACE},
            "spec": {"version": "0.3.3", "authSecretName": "registry-auth"},
        })
        self.wait("minimal AgentRegistry and managed PostgreSQL become Ready", self.ready, timeout=420)
        parent = self.get("agentregistry", NAME)
        self.parent_uid = parent["metadata"]["uid"]
        self.db_secret = self.get("secret", DB_NAME)
        self.db_data = {k: base64.b64decode(v).decode() for k, v in self.db_secret["data"].items()}
        self.sensitive.extend(self.db_data[key] for key in ("url", "password", "postgres-password"))
        self.db_statefulset = self.get("statefulset", DB_NAME)
        self.pvc = "data-" + DB_NAME + "-0"
        pvc = self.get("pvc", self.pvc)
        if pvc["status"]["phase"] != "Bound":
            raise RuntimeError("Managed database PVC is not Bound")
        for obj in [self.db_secret, self.db_statefulset]:
            if not any(o["uid"] == self.parent_uid for o in obj["metadata"].get("ownerReferences", [])):
                raise RuntimeError("Managed DB Secret/StatefulSet is not owned by AgentRegistry")
        self.evidence["database_image"] = self.db_statefulset["spec"]["template"]["spec"]["containers"][0]["image"]
        self.evidence["tenant_images"] = sorted({c["image"] for p in self.get("pods")["items"] for c in p["spec"]["containers"]})
        self.check("minimal CR creates owned database Secret, StatefulSet, and bound persistent storage")

    def sql(self, statement, *, admin=False):
        password = self.db_data["postgres-password" if admin else "password"]
        username = "postgres" if admin else self.db_data["username"]
        database = self.db_data["database"]
        # SQL and password travel on stdin, never as process arguments or logs.
        result = self.kubectl(
            "exec", "-i", "-n", NAMESPACE, DB_NAME + "-0", "--", "sh", "-ceu",
            'IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -h 127.0.0.1 -U "$1" -d "$2" -X -A -t -v ON_ERROR_STOP=1',
            "acceptance", username, database,
            input=password + "\n" + statement + "\n",
        )
        return result.stdout.strip()

    def inspect_database(self):
        extensions = self.sql("SELECT extname FROM pg_extension WHERE extname IN ('pg_trgm','vector') ORDER BY extname;")
        if extensions.splitlines() != ["pg_trgm", "vector"]:
            raise RuntimeError("Required PostgreSQL extensions are missing")
        permissions = self.sql("SELECT rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls FROM pg_roles WHERE rolname=current_user;")
        if permissions != "f|f|f|f|f":
            raise RuntimeError("Registry database role is overprivileged")
        self.sql("CREATE TABLE acceptance_persistence (value text PRIMARY KEY); INSERT INTO acceptance_persistence VALUES ('survives-restart');")
        self.check("pg_trgm/vector are installed and application login has no administrative role privileges")

    def inspect_admission(self):
        for database in [
            {"host": "external." + NAMESPACE + ".svc.cluster.local", "secretName": "external-db"},
            {"storage": "2Gi"},
        ]:
            result = self.kubectl(
                "patch", "agentregistry", NAME, "-n", NAMESPACE, "--type=merge",
                "--patch", json.dumps({"spec": {"database": database}}), "--dry-run=server",
                check=False,
            )
            if result.returncode == 0 or "immutable" not in result.stderr:
                raise RuntimeError("Database mode/storage change was not rejected by admission")
        for database in [
            {"host": "external." + NAMESPACE + ".svc.cluster.local"},
            {"host": "external." + NAMESPACE + ".svc.cluster.local", "secretName": "external-db", "storage": "2Gi"},
        ]:
            obj = {
                "apiVersion": "registry.deepai.cloud/v1alpha1", "kind": "AgentRegistry",
                "metadata": {"name": "invalid-database", "namespace": NAMESPACE},
                "spec": {"version": "0.3.3", "authSecretName": "registry-auth", "database": database},
            }
            result = self.kubectl("create", "--dry-run=server", "-f", "-", input=json.dumps(obj), check=False)
            if result.returncode == 0 or "Invalid" not in result.stderr:
                raise RuntimeError("Invalid external database settings were not rejected by admission")
        self.check("live admission rejects database mode/storage changes and invalid external settings")

    def http(self, authenticated):
        headers = {}
        if authenticated:
            headers["Authorization"] = "Basic " + base64.b64encode((self.username + ":" + self.password).encode()).decode()
        request = urllib.request.Request(f"http://127.0.0.1:{self.port}/v0/servers?limit=1", headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def start_forward(self):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            self.port = sock.getsockname()[1]
        self.forward_output = open(self.work / "port-forward.log", "w")
        self.forward = subprocess.Popen([
            "kubectl", "--kubeconfig", str(self.kubeconfig), "--context", self.context,
            "-n", NAMESPACE, "port-forward", "service/" + NAME,
            f"{self.port}:8080", "--address=127.0.0.1",
        ], stdout=self.forward_output, stderr=subprocess.STDOUT, env=self.env)

        def listening():
            if self.forward.poll() is not None:
                raise RuntimeError("Registry port-forward exited early")
            try:
                with socket.create_connection(("127.0.0.1", self.port), timeout=1):
                    return True
            except OSError:
                return False
        self.wait("registry port-forward", listening, timeout=20)

    def inspect_http(self):
        self.start_forward()
        if self.http(False)[0] != 401:
            raise RuntimeError("Unauthenticated catalog access was not rejected")
        status, body = self.http(True)
        if status != 200 or not isinstance(json.loads(body).get("servers"), list):
            raise RuntimeError("Authenticated catalog request failed")
        self.check("ready registry rejects anonymous HTTP and serves authenticated catalog requests")

    def restart_database(self):
        pod = self.get("pod", DB_NAME + "-0")
        old_uid = pod["metadata"]["uid"]
        self.kubectl("delete", "pod", DB_NAME + "-0", "-n", NAMESPACE, "--wait=true", "--timeout=120s", timeout=130)

        def restarted():
            result = self.kubectl("get", "pod", DB_NAME + "-0", "-n", NAMESPACE, "-o", "json", check=False)
            if result.returncode:
                return False
            pod = json.loads(result.stdout)
            return pod["metadata"]["uid"] != old_uid and any(c["type"] == "Ready" and c["status"] == "True" for c in pod.get("status", {}).get("conditions", []))

        self.wait("PostgreSQL pod restart", restarted)
        self.wait("registry recovers database readiness", self.ready)
        self.wait("authenticated API recovers after database restart", lambda: self.http(True)[0] == 200)
        current = self.get("secret", DB_NAME)
        if current["metadata"]["uid"] != self.db_secret["metadata"]["uid"] or current["data"] != self.db_secret["data"]:
            raise RuntimeError("Managed credentials changed across reconciliation/restart")
        if self.sql("SELECT value FROM acceptance_persistence;") != "survives-restart":
            raise RuntimeError("Database content did not survive pod restart")
        self.check("database pod restart preserves credentials and data, and registry HTTP recovers")

    def inspect_cleanup(self):
        self.kubectl("delete", "agentregistry", NAME, "-n", NAMESPACE, "--wait=true", "--timeout=120s", timeout=130)

        def children_gone():
            items = self.get("deployments,statefulsets,services,serviceaccounts,configmaps,networkpolicies,secrets")["items"]
            return not any(any(o["uid"] == self.parent_uid for o in obj["metadata"].get("ownerReferences", [])) for obj in items)

        self.wait("managed children removed after registry deletion", children_gone)
        pvc = self.get("pvc", self.pvc)
        if pvc["status"]["phase"] != "Bound" or pvc["metadata"].get("deletionTimestamp"):
            raise RuntimeError("Database PVC was not retained")
        self.get("secret", "registry-auth")
        self.check("deletion removes managed children and retains database PVC and user-supplied auth Secret")

    def inspect_recovery(self):
        self.apply({
            "apiVersion": "registry.deepai.cloud/v1alpha1", "kind": "AgentRegistry",
            "metadata": {"name": NAME, "namespace": NAMESPACE},
            "spec": {"version": "0.3.3", "authSecretName": "registry-auth"},
        })

        def blocked():
            parent = self.get("agentregistry", NAME)
            conditions = parent.get("status", {}).get("conditions", [])
            status = {c["type"]: c["status"] for c in conditions}
            result = self.kubectl("get", "statefulset", DB_NAME, "-n", NAMESPACE, "-o", "json", check=False)
            return result.returncode == 0 and json.loads(result.stdout)["spec"]["replicas"] == 0 and status.get("Ready") == "False" and status.get("DependenciesReady") == "False"

        self.wait("retained PVC blocks password regeneration", blocked)
        result = self.kubectl("get", "secret", DB_NAME, "-n", NAMESPACE, "--ignore-not-found", "-o", "name")
        if result.stdout.strip():
            raise RuntimeError("Recreating a registry silently generated new credentials over retained data")
        self.check("recreating a registry over retained storage fails closed without regenerating credentials")
        parent = self.get("agentregistry", NAME)
        self.apply({
            "apiVersion": "v1", "kind": "Secret",
            "metadata": {
                "name": DB_NAME, "namespace": NAMESPACE,
                "labels": {"controller-uid": parent["metadata"]["uid"]},
            },
            "type": self.db_secret["type"], "immutable": self.db_secret["immutable"],
            "data": self.db_secret["data"],
        })
        self.wait("restored database Secret recovers retained database", self.ready)
        self.parent_uid = self.get("agentregistry", NAME)["metadata"]["uid"]
        restored = self.get("secret", DB_NAME)
        if not any(owner["uid"] == self.parent_uid for owner in restored["metadata"].get("ownerReferences", [])):
            raise RuntimeError("Restored Secret was not adopted by the recreated AgentRegistry")
        if self.sql("SELECT value FROM acceptance_persistence;") != "survives-restart":
            raise RuntimeError("Restored database lost its saved content")
        self.check("restoring original credentials with the current parent selector adopts the Secret and recovers saved data")
        self.inspect_cleanup()

    def diagnostics(self):
        if not self.kubeconfig.exists():
            return
        # Do not dump Secrets, pod environments, or logs containing connection URLs.
        for namespace in [SYSTEM_NAMESPACE, NAMESPACE]:
            result = self.kubectl("get", "pods,statefulsets,deployments,pvc", "-n", namespace, "-o", "wide", check=False)
            say(self.sanitize(result.stdout))
        result = self.kubectl("get", "agentregistry", NAME, "-n", NAMESPACE, "-o", "jsonpath={.status.conditions}", check=False)
        say(self.sanitize(result.stdout))

    def cleanup(self):
        if self.forward:
            self.forward.terminate()
            try:
                self.forward.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.forward.kill()
                self.forward.wait(timeout=10)
        if self.forward_output:
            self.forward_output.close()
        if self.created:
            say("Removing owned cluster " + self.cluster)
            self.run([self.k3d, "cluster", "delete", self.cluster], timeout=150)
        self.kubeconfig.unlink(missing_ok=True)


def main():
    os.umask(0o077)
    test = Acceptance()
    succeeded = False
    try:
        test.prerequisites()
        test.create_cluster()
        test.install()
        test.create_registry()
        test.inspect_admission()
        test.inspect_database()
        test.inspect_http()
        test.restart_database()
        test.inspect_cleanup()
        test.inspect_recovery()
        succeeded = True
    except (Exception, KeyboardInterrupt) as error:
        say("FAIL " + test.sanitize(str(error)))
        test.diagnostics()
    finally:
        try:
            test.cleanup()
        except Exception as error:
            succeeded = False
            say("Cleanup failed: " + test.sanitize(str(error)))
        test.evidence["passed"] = succeeded
        (test.work / "result.json").write_text(json.dumps(test.evidence, indent=2) + "\n")
        say("Non-secret acceptance evidence: " + str(test.work / "result.json"))
    return 0 if succeeded else 1


if __name__ == "__main__":
    sys.exit(main())
