#!/usr/bin/env python3
"""Check the bundled chart offline using Helm and the Python standard library."""
import json
import os
from pathlib import Path
import re
import runpy
import shutil
import subprocess
import tarfile
import tempfile
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "charts/agentregistry-operator"
HELM = os.environ.get("HELM") or shutil.which("helm")
KUBE_VERSION = "1.34.0"
OPERATOR_NAMESPACE = "registry-system"


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def helm(*args):
    result = subprocess.run([HELM, *map(str, args)], cwd=ROOT, text=True, capture_output=True)
    require(result.returncode == 0, f"helm {' '.join(map(str, args))}\n{result.stdout}{result.stderr}")
    return result.stdout


def rendered_documents(chart, release, settings):
    args = ["template", release, chart, "--namespace", OPERATOR_NAMESPACE, "--kube-version", KUBE_VERSION, "--include-crds"]
    for setting in settings:
        args.extend(["--set", setting])
    output = helm(*args)
    documents = []
    for section in re.split(r"^---\s*$", output, flags=re.MULTILINE):
        source = re.search(r"^# Source: (.+)$", section, flags=re.MULTILINE)
        body = re.sub(r"^#.*$", "", section, flags=re.MULTILINE).strip()
        if body:
            documents.append((source[1] if source else "", body))
    return documents


def check_database_contract(by_kind):
    """Check declarations that make managed PostgreSQL reconcilable and durable."""
    controller = by_kind["CompositeController"]
    children = {(child["apiVersion"], child["resource"]): child for child in controller["spec"]["childResources"]}
    expected = {("apps/v1", "statefulsets"), ("v1", "secrets"), ("v1", "configmaps")}
    require(expected <= children.keys(), "CompositeController must reconcile PostgreSQL workloads, credentials and initialization")
    for key in expected:
        require(children[key]["updateStrategy"]["method"] == "InPlace", f"PostgreSQL child must reconcile in place: {key}")
    require(("v1", "persistentvolumeclaims") not in children, "Retained PostgreSQL PVCs must not be disposable CompositeController children")

    def grants(group, resource):
        return {verb for rule in by_kind["ClusterRole"]["rules"]
                if group in rule["apiGroups"] and resource in rule["resources"]
                for verb in rule["verbs"]}

    reads = {"get", "list", "watch"}
    writes = reads | {"create", "update", "patch", "delete"}
    for group, resource in (("apps", "statefulsets"), ("", "secrets"), ("", "configmaps")):
        require(writes <= grants(group, resource), f"Missing controller permissions for PostgreSQL child: {resource}")
    require(grants("", "persistentvolumeclaims") == reads, "PostgreSQL PVC access must remain read-only for credential recovery checks")

    crd = by_kind["CustomResourceDefinition"]
    version = next(version for version in crd["spec"]["versions"] if version["name"] == "v1alpha1")
    spec = version["schema"]["openAPIV3Schema"]["properties"]["spec"]
    require("database" not in spec.get("required", []), "Omitting database must select managed PostgreSQL")
    database = spec["properties"]["database"]
    require(not database.get("required"), "Managed PostgreSQL must not require external database fields")
    require(not database.get("default"), "Schema must not default to external database settings")
    props = database["properties"]
    require({"host", "secretName", "storage", "storageClassName"} <= props.keys(), "Database schema is incomplete")
    pattern = props["storage"]["pattern"]
    for valid in ("1Mi", "1Gi", "10Gi"):
        require(re.fullmatch(pattern, valid), f"Managed storage rejects valid size: {valid}")
    for invalid in ("0Gi", "-1Gi", "1", "1Ti", "1.5Gi"):
        require(not re.fullmatch(pattern, invalid), f"Managed storage accepts unsupported size: {invalid}")
    require(re.fullmatch(props["storageClassName"]["pattern"], ""), "Explicit empty storageClassName must remain available")
    rules = {validation["rule"] for validation in database["x-kubernetes-validations"]}
    require("has(self.host) == has(self.secretName)" in rules, "External database fields must be supplied together")
    require("!has(self.host) || (!has(self.storage) && !has(self.storageClassName))" in rules, "External mode must reject managed storage settings")
    transition_rules = {validation["rule"] for validation in spec["x-kubernetes-validations"]}
    require("has(self.database) == has(oldSelf.database) && (!has(self.database) || self.database == oldSelf.database)" in transition_rules,
            "Database mode and storage must remain immutable, including transitions from omitted settings")


def check_release(chart, release="registry", settings=(), namespace="registry-system", name="metacontroller", account=None, create_account=True):
    documents = rendered_documents(chart, release, settings)
    native = [json.loads(body) for _, body in documents if body.startswith("{")]
    by_kind = {doc["kind"]: doc for doc in native}
    required_kinds = {
        "CustomResourceDefinition", "Deployment", "Service", "ServiceAccount",
        "NetworkPolicy", "ClusterRole", "ClusterRoleBinding", "CompositeController",
    }
    require(set(by_kind) == required_kinds and len(native) == len(required_kinds),
            "The parent chart must render exactly one of each expected resource")
    crd = by_kind["CustomResourceDefinition"]
    parent = by_kind["CompositeController"]["spec"]["parentResource"]
    require(crd["spec"]["scope"] == "Namespaced", "AgentRegistry resources must remain namespace-scoped")
    require(parent["resource"] == crd["spec"]["names"]["plural"], "Controller watches the wrong parent resource")
    require(parent["apiVersion"] in {
        crd["spec"]["group"] + "/" + version["name"]
        for version in crd["spec"]["versions"] if version["served"]
    }, "Controller parent API must be served by the packaged CRD")
    upstream = {Path(source).name: body for source, body in documents if "/charts/metacontroller/templates/" in source}
    require("statefulset.yaml" in upstream, "Bundled Metacontroller workload is missing")
    require(not any("clusterrole" in filename for filename in upstream), "Upstream wildcard or aggregated RBAC must stay disabled")
    workload = upstream["statefulset.yaml"]
    require('image: "ghcr.io/metacontroller/metacontroller:v4.17.2"' in workload, "Unexpected Metacontroller image version")
    require(f"\n  namespace: {namespace}\n" in workload, "Workload namespace mismatch")
    account = account or f"{release}-{name}"
    require(f"\n      serviceAccountName: {account}\n" in workload, "Workload service account mismatch")
    require(workload.count(f"app.kubernetes.io/name: {name}\n") == 3, "Workload selectors/name labels mismatch")
    require(workload.count(f"app.kubernetes.io/instance: {release}\n") == 3, "Workload selectors/release labels mismatch")
    if create_account:
        service_account = upstream.get("serviceaccount.yaml", "")
        require(f"\n  name: {account}\n" in service_account, "Bundled service account mismatch")
        require(f"\n  namespace: {namespace}\n" in service_account, "Service account namespace mismatch")
    else:
        require("serviceaccount.yaml" not in upstream, "Existing service account should not be recreated")
    binding = by_kind["ClusterRoleBinding"]
    require(binding["subjects"] == [{"kind": "ServiceAccount", "name": account, "namespace": namespace}], "RBAC subject does not match bundled workload")
    role = by_kind["ClusterRole"]
    require(binding["roleRef"]["name"] == role["metadata"]["name"], "RBAC binding references the wrong role")
    for rule in role["rules"]:
        for field in ("apiGroups", "resources", "verbs", "nonResourceURLs"):
            require(not any("*" in value for value in rule.get(field, [])), "Explicit RBAC must not contain wildcards")
    check_database_contract(by_kind)
    policy = by_kind["NetworkPolicy"]
    peer = policy["spec"]["ingress"][0]["from"][0]
    require(namespace == OPERATOR_NAMESPACE, "Metacontroller must share the operator namespace")
    require(set(peer) == {"podSelector"}, "Webhook ingress must select only namespace-local controller pods")
    require(peer["podSelector"]["matchLabels"] == {"app.kubernetes.io/name": name, "app.kubernetes.io/instance": release}, "Network policy selectors do not match workload")
    require(policy["metadata"]["namespace"] == OPERATOR_NAMESPACE, "Webhook policy must stay in the operator namespace")
    require(policy["spec"]["policyTypes"] == ["Ingress", "Egress"]
            and policy["spec"]["egress"] == [], "Webhook must have no egress")
    for doc in native:
        if doc["kind"] in {"Deployment", "Service", "ServiceAccount", "NetworkPolicy"}:
            require(doc["metadata"]["namespace"] == OPERATOR_NAMESPACE,
                    "All namespaced control-plane resources must share the release namespace")
    deployment = by_kind["Deployment"]
    pod = deployment["spec"]["template"]
    service = by_kind["Service"]
    service_account = by_kind["ServiceAccount"]
    require(pod["spec"]["serviceAccountName"] == service_account["metadata"]["name"],
            "Webhook pod must use the packaged service account")
    require(pod["spec"]["automountServiceAccountToken"] is False
            and service_account["automountServiceAccountToken"] is False,
            "Webhook must not receive a Kubernetes API token")
    for selector in (deployment["spec"]["selector"]["matchLabels"],
                     service["spec"]["selector"], policy["spec"]["podSelector"]["matchLabels"]):
        require(all(pod["metadata"]["labels"].get(key) == value for key, value in selector.items()),
                "Webhook deployment, service, and network policy must select its pods")
    container = pod["spec"]["containers"][0]
    gateway_image = next(item["value"] for item in container["env"] if item["name"] == "GATEWAY_IMAGE")
    require(gateway_image == container["image"], "Webhook and managed gateway images must stay identical")
    port = next(port for port in service["spec"]["ports"] if port["name"] == "http")
    target_port = next(item["containerPort"] for item in container["ports"] if item["name"] == port["targetPort"])
    require(any(item["port"] == target_port and item["protocol"] == "TCP"
                for item in policy["spec"]["ingress"][0]["ports"]),
            "Webhook network policy must admit traffic to the service's container port")
    for hook_name, hook in by_kind["CompositeController"]["spec"]["hooks"].items():
        url = urlsplit(hook["webhook"]["url"])
        require(url.hostname == f"{service['metadata']['name']}.{OPERATOR_NAMESPACE}.svc.cluster.local"
                and url.port == port["port"] and url.path == f"/{hook_name}",
                f"The {hook_name} hook must reach the packaged webhook service")
    crds = [body for _, body in documents if re.search(r"^kind: CustomResourceDefinition$", body, re.MULTILINE)]
    for resource in ("compositecontrollers", "decoratorcontrollers", "controllerrevisions"):
        require(any(f"  name: {resource}.metacontroller.k8s.io\n" in crd for crd in crds), f"Missing bundled {resource} CRD")
    return by_kind


def main():
    require(HELM is not None, "Helm is required; install it or set HELM to its path")
    pinned = runpy.run_path(str(ROOT / "scripts/fetch-metacontroller.py"))
    archive = pinned["DEST"]
    require(archive.is_file(), f"Missing vendored chart: {archive}")
    require(pinned["digest"](archive.read_bytes()) == pinned["ARCHIVE"], "Vendored chart archive checksum mismatch")
    with tarfile.open(archive) as vendor:
        metadata = vendor.extractfile("metacontroller-helm/Chart.yaml").read().decode()
        require(f"version: {pinned['VERSION']}\n" in metadata, "Vendored chart version mismatch")
        require(f"appVersion: v{pinned['VERSION']}\n" in metadata, "Vendored application version mismatch")
    helm("lint", CHART, "--strict")
    default_release = check_release(CHART)
    default_image = default_release["Deployment"]["spec"]["template"]["spec"]["containers"][0]["image"]
    metadata = (CHART / "Chart.yaml").read_text()
    app_version = re.search(r'^appVersion: "([^"]+)"$', metadata, re.MULTILINE)[1]
    require(default_image == f"ghcr.io/deepai-cloud/agentregistry-operator:v{app_version}",
            "Default install must use the published image matching the chart appVersion")
    # Exercise upstream naming helpers and existing accounts in the release namespace.
    check_release(CHART, release="production", settings=(
        "metacontroller.nameOverride=control-plane",
        "metacontroller.fullnameOverride=custom-controller",
    ), name="control-plane", account="custom-controller")
    check_release(CHART, release="metacontroller-prod", account="metacontroller-prod")
    check_release(CHART, settings=("metacontroller.serviceAccount.name=controller-identity",), account="controller-identity")
    check_release(CHART, settings=("metacontroller.serviceAccount.create=false", "metacontroller.serviceAccount.name=existing-controller"), account="existing-controller", create_account=False)
    # Immutable references must be used end to end, rather than retaining the tag
    # in either the webhook or the tenant gateway image reference.
    digest = "sha256:" + "a" * 64
    repository = "registry.example.test/team/agentregistry-operator"
    pinned_release = check_release(CHART, settings=(
        f"image.repository={repository}", f"image.digest={digest}", "image.tag=ignored-tag",
    ))
    pinned_image = pinned_release["Deployment"]["spec"]["template"]["spec"]["containers"][0]["image"]
    require(pinned_image == f"{repository}@{digest}", "Image digest must take precedence over tag")
    check_release(CHART, settings=(f"image.digest={digest}", "image.tag="))
    # Unsafe overrides must fail validation instead of rendering broad permissions
    # or multiple controllers without the required leader-election grants.
    for setting in (
        "metacontroller.rbac.create=true", "metacontroller.replicas=2",
        "metacontroller.namespaceOverride=controllers",
        "metacontroller.serviceAccount.create=false", "replicaCount=0",
        "image.repository=", "image.digest=not-a-digest", "image.pullPolicy=Sometimes", "image.tag=",
    ):
        result = subprocess.run([HELM, "template", "registry", str(CHART), "--kube-version", KUBE_VERSION, "--set", setting], cwd=ROOT, text=True, capture_output=True)
        require(result.returncode != 0 and "values don't meet" in result.stderr, f"Invalid values must fail schema validation: {setting}")
    unsupported = subprocess.run([HELM, "template", "registry", str(CHART), "--kube-version", "1.29.0"],
                                 cwd=ROOT, text=True, capture_output=True)
    require(unsupported.returncode != 0 and "incompatible with Kubernetes" in unsupported.stderr,
            "Kubernetes versions below the supported minimum must fail before installation")
    # Packaging must preserve the dependency and CRDs without a dependency download.
    with tempfile.TemporaryDirectory(prefix="agentregistry-chart-") as directory:
        helm("package", CHART, "--destination", directory)
        packages = list(Path(directory).glob("*.tgz"))
        require(len(packages) == 1, "Expected one packaged operator chart")
        check_release(packages[0])
    print("Chart verified: strict lint, pinned dependency, bundled CRDs/workload, explicit RBAC, "
          "identity/namespace selectors, webhook connectivity, image digests, values validation, "
          "Kubernetes minimum, PostgreSQL controller/schema contract, offline packaging")


if __name__ == "__main__":
    main()
