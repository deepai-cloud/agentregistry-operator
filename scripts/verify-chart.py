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
CONTROLLER_SERVICE_SETTINGS = (
    "metacontroller.service.enabled=true",
    "metacontroller.service.ports[0].name=metrics",
    "metacontroller.service.ports[0].port=9999",
    "metacontroller.service.ports[0].targetPort=9999",
)


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


def check_label_values(value):
    """Check rendered label values, including selectors and pod templates."""
    if isinstance(value, list):
        for item in value:
            check_label_values(item)
    elif isinstance(value, dict):
        for key, item in value.items():
            if key in {"labels", "matchLabels"} and isinstance(item, dict):
                for name, label in item.items():
                    require(isinstance(label, str) and len(label) <= 63
                            and (label == "" or re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?", label)),
                            f"Invalid Kubernetes label value for {name}: {label!r}")
            check_label_values(item)


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
    classes = props["storageClassName"]
    cases = json.loads((ROOT / "internal/operator/testdata/database-storage-validation.json").read_text())
    for case in cases:
        if "storageClassName" not in case["database"]:
            continue
        name = case["database"]["storageClassName"]
        accepted = len(name) <= classes["maxLength"] and re.fullmatch(classes["pattern"], name) is not None
        require(accepted == case["valid"], f"StorageClass admission disagrees with operator validation: {case['name']}")
    rules = {validation["rule"] for validation in database["x-kubernetes-validations"]}
    require("has(self.host) == has(self.secretName)" in rules, "External database fields must be supplied together")
    require("!has(self.host) || (!has(self.storage) && !has(self.storageClassName))" in rules, "External mode must reject managed storage settings")
    transition_rules = {validation["rule"] for validation in spec["x-kubernetes-validations"]}
    require("has(self.database) == has(oldSelf.database) && (!has(self.database) || self.database == oldSelf.database)" in transition_rules,
            "Database mode and storage must remain immutable, including transitions from omitted settings")


def check_exposure_hostnames(crd):
    version = next(version for version in crd["spec"]["versions"] if version["name"] == "v1alpha1")
    spec = version["schema"]["openAPIV3Schema"]["properties"]["spec"]
    hostname = spec["properties"]["exposure"]["properties"]["hostname"]
    cases = json.loads((ROOT / "internal/operator/testdata/exposure-hostname-validation.json").read_text())
    for case in cases:
        name = case["hostname"]
        accepted = hostname["minLength"] <= len(name) <= hostname["maxLength"] and re.fullmatch(hostname["pattern"], name) is not None
        if accepted and "not" in hostname:
            # IPv6 is already excluded by DNS syntax. Reject canonical IPv4
            # literals while retaining numeric DNS labels that are not IPs.
            accepted = re.fullmatch(hostname["not"]["pattern"], name) is None
        require(accepted == case["valid"], f"Hostname admission disagrees with operator validation: {case['name']}")


def check_secret_names(crd):
    version = next(version for version in crd["spec"]["versions"] if version["name"] == "v1alpha1")
    props = version["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]
    fields = {"authSecretName": props["authSecretName"],
              "database.secretName": props["database"]["properties"]["secretName"],
              "exposure.tlsSecretName": props["exposure"]["properties"]["tlsSecretName"]}
    cases = json.loads((ROOT / "internal/operator/testdata/secret-name-validation.json").read_text())
    for field, schema in fields.items():
        for case in cases:
            name = case["secret"]
            accepted = schema["minLength"] <= len(name) <= schema["maxLength"] and re.fullmatch(schema["pattern"], name) is not None
            require(accepted == case["valid"], f"Secret admission disagrees with operator validation: {field}, {case['name']}")


def check_release(chart, release="registry", settings=(), namespace="registry-system", name="metacontroller", account=None, create_account=True, controller_service=None):
    documents = rendered_documents(chart, release, settings)
    native = [json.loads(body) for _, body in documents if body.startswith("{")]
    for document in native:
        check_label_values(document)
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
    controller_name = re.search(r"(?m)^metadata:\n  name: ([^\n]+)\n", workload)
    require(controller_name is not None, "Missing Metacontroller StatefulSet name")
    controller_name = controller_name[1]
    require(len(controller_name + "-0") <= 63
            and re.fullmatch(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?", controller_name),
            f"Metacontroller StatefulSet cannot create a valid pod hostname: {controller_name}-0")
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
    if controller_service is None:
        require("service.yaml" not in upstream, "Metacontroller Service should remain disabled")
    else:
        controller_endpoint = upstream.get("service.yaml", "")
        require(len(controller_service) <= 63
                and re.fullmatch(r"[a-z](?:[-a-z0-9]*[a-z0-9])?", controller_service),
                f"Invalid Metacontroller Service name: {controller_service}")
        require(f"\n  name: {controller_service}\n" in controller_endpoint,
                "Metacontroller Service name mismatch")
        require(f"\n  namespace: {namespace}\n" in controller_endpoint,
                "Metacontroller Service namespace mismatch")
        require(f"\n    app.kubernetes.io/name: {name}\n" in controller_endpoint,
                "Metacontroller Service must select its controller pods")
        require(controller_service != by_kind["Service"]["metadata"]["name"],
                "Metacontroller Service must not replace the webhook Service")
    binding = by_kind["ClusterRoleBinding"]
    require(binding["subjects"] == [{"kind": "ServiceAccount", "name": account, "namespace": namespace}], "RBAC subject does not match bundled workload")
    role = by_kind["ClusterRole"]
    require(binding["roleRef"]["name"] == role["metadata"]["name"], "RBAC binding references the wrong role")
    for rule in role["rules"]:
        for field in ("apiGroups", "resources", "verbs", "nonResourceURLs"):
            require(not any("*" in value for value in rule.get(field, [])), "Explicit RBAC must not contain wildcards")
    check_database_contract(by_kind)
    check_exposure_hostnames(by_kind["CustomResourceDefinition"])
    check_secret_names(by_kind["CustomResourceDefinition"])
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
    service_name = service["metadata"]["name"]
    require(service_name == release and len(service_name) <= 63
            and re.fullmatch(r"[a-z](?:[-a-z0-9]*[a-z0-9])?", service_name),
            f"Webhook Service must have a valid name matching the release: {service_name}")
    service_account = by_kind["ServiceAccount"]
    require(account != service_account["metadata"]["name"],
            "Metacontroller must not share the webhook's token-disabled service account")
    require(pod["spec"]["serviceAccountName"] == service_account["metadata"]["name"],
            "Webhook pod must use the packaged service account")
    require(pod["spec"]["automountServiceAccountToken"] is False
            and service_account["automountServiceAccountToken"] is False,
            "Webhook must not receive a Kubernetes API token")
    controller_labels = {"app.kubernetes.io/name": name, "app.kubernetes.io/instance": release}
    for selector in (deployment["spec"]["selector"]["matchLabels"],
                     service["spec"]["selector"], policy["spec"]["podSelector"]["matchLabels"]):
        require(all(pod["metadata"]["labels"].get(key) == value for key, value in selector.items()),
                "Webhook deployment, service, and network policy must select its pods")
        require(any(controller_labels.get(key) != value for key, value in selector.items()),
                "Webhook deployment, service, and network policy must exclude Metacontroller pods")
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


def check_colliding_overrides(chart):
    cases = [
        ("metacontroller-prod", (), "service account"),
        ("registry", ("metacontroller.nameOverride=reg",), "service account"),
        ("registry", ("metacontroller.fullnameOverride=registry",), "service account"),
        ("registry", ("metacontroller.fullnameOverride=registry-",), "service account"),
        ("registry", ("metacontroller.serviceAccount.name=registry",), "service account"),
        ("registry", ("metacontroller.serviceAccount.create=false",
                      "metacontroller.serviceAccount.name=registry"), "service account"),
        ("registry", ("metacontroller.nameOverride=agentregistry-operator",), "pod selectors"),
        ("registry", ("metacontroller.nameOverride=agentregistry-operator-",), "pod selectors"),
    ]
    for release, naming in (
        ("registry", "metacontroller.fullnameOverride=registry"),
        ("registry", "metacontroller.fullnameOverride=registry-"),
        ("registry", "metacontroller.nameOverride=reg"),
        ("metacontroller-prod", "metacontroller.nameOverride=metacontroller"),
    ):
        # A separate service account avoids that earlier guard, but the optional
        # Service would still share the webhook's resource identity.
        settings = CONTROLLER_SERVICE_SETTINGS + (naming, "metacontroller.serviceAccount.name=controller-identity")
        cases.append((release, settings, "service"))
    for release, settings, reason in cases:
        args = [HELM, "template", release, str(chart), "--namespace", OPERATOR_NAMESPACE,
                "--kube-version", KUBE_VERSION]
        for setting in settings:
            args.extend(["--set", setting])
        result = subprocess.run(args, cwd=ROOT, text=True, capture_output=True)
        require(result.returncode != 0 and f"Metacontroller {reason} must differ from the webhook" in result.stderr,
                f"Colliding names must fail rendering with an actionable error: {release}, {settings}\n{result.stderr}")


def check_packaged_versions(directory):
    # Exercise both the version label and truncation of the longer chart label.
    chart_prefix = len(CHART.name) + 1
    versions = ["1.2.3", "1.2.3-rc-", "1.2.3-rc---", "1.2.3-" + "a" * 58,
                "1.2.3-" + "a" * 56 + ".b"]
    versions += ["1.2.3-" + "a" * (62 - chart_prefix - 6) + separator + "b"
                 for separator in (".", "--")]
    for version in versions:
        helm("package", CHART, "--version", version, "--app-version", version,
             "--destination", directory)
        archive = Path(directory) / f"agentregistry-operator-{version}.tgz"
        resources = check_release(archive, settings=(f"image.tag=v{version}",))
        for kind, resource in resources.items():
            if kind != "CustomResourceDefinition":
                require(resource["metadata"].get("annotations", {}).get("registry.deepai.cloud/operator-version") == version,
                        f"{kind} must preserve the complete operator version: {version}")
        image = resources["Deployment"]["spec"]["template"]["spec"]["containers"][0]["image"]
        require(image == f"ghcr.io/deepai-cloud/agentregistry-operator:v{version}",
                "Label normalization must not change the requested image tag")


def check_controller_names(chart):
    # StatefulSet pods inherit <name>-0 as their DNS hostname. Upstream's
    # 63-character truncation only considers the StatefulSet's metadata name.
    max_default_release = 61 - len("-metacontroller")
    cases = [("r" * size, ()) for size in (max_default_release + 1, max_default_release + 2, 53)]
    cases += [("registry", (f"metacontroller.fullnameOverride={name}",))
              for name in ("a" * 62, "a" * 63, "a" * 80, "control.plane", "Controller", "controller---")]
    cases += [("registry", ("metacontroller.nameOverride=" + "c" * 54,))]
    for release, settings in cases:
        args = [HELM, "template", release, str(chart), "--namespace", OPERATOR_NAMESPACE,
                "--kube-version", KUBE_VERSION]
        for setting in settings:
            args.extend(["--set", setting])
        result = subprocess.run(args, cwd=ROOT, text=True, capture_output=True)
        require(result.returncode != 0
                and "Metacontroller StatefulSet name must be a DNS label of at most 61 characters" in result.stderr
                and "metacontroller.fullnameOverride" in result.stderr,
                f"Invalid controller names must fail before installation: {release}, {settings}\n{result.stderr}")
    # Exercise the exact limit, a long release with an explicit repair, upstream
    # trailing-hyphen trimming, and a valid hostname starting with a digit.
    check_release(chart, release="r" * max_default_release)
    for release, override, resolved in (
        ("registry", "a" * 61, "a" * 61),
        ("r" * 53, "controller", "controller"),
        ("registry", "a" * 61 + "-", "a" * 61),
        ("registry", "1controller", "1controller"),
    ):
        check_release(chart, release=release, settings=(f"metacontroller.fullnameOverride={override}",), account=resolved)


def check_service_names(chart):
    # Helm allows DNS subdomains and digit-leading release names, but Services
    # on the minimum supported Kubernetes version require DNS1035 labels.
    cases = [(release, (), "Helm release name must be a lowercase DNS label beginning with a letter")
             for release in ("1registry", "123", "1-registry")]
    cases.append(("registry.prod", ("metacontroller.fullnameOverride=controller",),
                  "Helm release name must be a lowercase DNS label beginning with a letter"))
    cases += [("registry", CONTROLLER_SERVICE_SETTINGS + (f"metacontroller.fullnameOverride={name}",),
               "Metacontroller Service name must begin with a letter")
              for name in ("1controller", "1-controller")]
    for release, settings, error in cases:
        args = [HELM, "template", release, str(chart), "--namespace", OPERATOR_NAMESPACE,
                "--kube-version", KUBE_VERSION]
        for setting in settings:
            args.extend(["--set", setting])
        result = subprocess.run(args, cwd=ROOT, text=True, capture_output=True)
        require(result.returncode != 0 and error in result.stderr,
                f"Invalid Service names must fail before installation: {release}, {settings}\n{result.stderr}")
    for release, controller in (("r", "c"), ("registry-1", "controller-1")):
        check_release(chart, release=release, settings=CONTROLLER_SERVICE_SETTINGS + (
            f"metacontroller.fullnameOverride={controller}",
        ), account=controller, controller_service=controller)


def check_controller_labels(chart):
    # A valid fullnameOverride bypasses the StatefulSet name guard. The separate
    # name helper still populates labels and selectors, unquoted by upstream.
    cases = [(name, "Metacontroller name label must be a nonempty Kubernetes label value")
             for name in ("-controller", "controller_", "control/plane", "controller---", "-",
                          "a" * 62 + ".b", "a" * 62 + "_b")]
    cases += [(name, "Metacontroller name label must remain a string in the bundled chart YAML")
              for name in ("123", "1e3", "true", "false", "yes", "on", "null")]
    for name, message in cases:
        result = subprocess.run([
            HELM, "template", "registry", str(chart), "--namespace", OPERATOR_NAMESPACE,
            "--kube-version", KUBE_VERSION, "--set", "metacontroller.fullnameOverride=controller",
            "--set-string", "metacontroller.nameOverride=" + name,
        ], cwd=ROOT, text=True, capture_output=True)
        require(result.returncode != 0 and message in result.stderr and "metacontroller.nameOverride" in result.stderr,
                f"Invalid controller labels must fail before installation: {name!r}\n{result.stderr}")
    for override, resolved in (("Control_Plane.v2", "Control_Plane.v2"), ("1controller", "1controller"),
                               ("a" * 80, "a" * 63), ("controller-", "controller")):
        check_release(chart, settings=("metacontroller.fullnameOverride=controller",
                                       "metacontroller.nameOverride=" + override),
                      name=resolved, account="controller")


def check_controller_service_accounts(chart):
    # Both newly created accounts and references to existing accounts must be
    # valid names. The dependency does not truncate this particular override.
    invalid = ("Controller", "controller_identity", "controller/identity", "controller..identity",
               "-controller", "controller-", "controller.-identity", "controller.identity-",
               "a" * 254)
    for create in (True, False):
        for name in invalid:
            result = subprocess.run([
                HELM, "template", "registry", str(chart), "--kube-version", KUBE_VERSION,
                "--set", "metacontroller.serviceAccount.create=" + str(create).lower(),
                "--set-string", "metacontroller.serviceAccount.name=" + name,
            ], cwd=ROOT, text=True, capture_output=True)
            require(result.returncode != 0
                    and "Metacontroller service account name must be a lowercase DNS subdomain of at most 253 characters" in result.stderr
                    and "metacontroller.serviceAccount.name" in result.stderr,
                    f"Invalid service account must fail before installation: {create}, {name!r}\n{result.stderr}")
        for name in ("controller-identity", "controller.identity", "1controller", "a" * 64,
                     ".".join(["a" * 63] * 3 + ["a" * 61])):
            check_release(chart, settings=("metacontroller.serviceAccount.create=" + str(create).lower(),
                                           "metacontroller.serviceAccount.name=" + name),
                          account=name, create_account=create)


def check_yaml_identity_types(chart):
    # --set-string controls the values type, but upstream interpolates these
    # fields into YAML without quoting, so Kubernetes can still see scalars.
    cases = []
    for name in ("123", "1e3", "true", "false", "yes", "on", "null"):
        cases.append(("registry", OPERATOR_NAMESPACE,
                      ("metacontroller.fullnameOverride=" + name,
                       "metacontroller.serviceAccount.name=controller-identity"), (),
                      "Metacontroller StatefulSet name", "metacontroller.fullnameOverride"))
        for create in ("true", "false"):
            cases.append(("registry", OPERATOR_NAMESPACE,
                          ("metacontroller.serviceAccount.name=" + name,),
                          ("metacontroller.serviceAccount.create=" + create,),
                          "Metacontroller service account name", "metacontroller.serviceAccount.name"))
        cases.append(("registry", name, (), (), "Helm release namespace", "--namespace"))
        if name[0].isalpha():
            cases.append((name, OPERATOR_NAMESPACE, (), (), "Helm release name label", "release name"))
    for release, namespace, strings, settings, field, hint in cases:
        args = [HELM, "template", release, str(chart), "--namespace", namespace, "--kube-version", KUBE_VERSION]
        for value in strings:
            args.extend(["--set-string", value])
        for value in settings:
            args.extend(["--set", value])
        result = subprocess.run(args, cwd=ROOT, text=True, capture_output=True)
        require(result.returncode != 0 and f"{field} must remain a string in the bundled chart YAML" in result.stderr
                and hint in result.stderr,
                f"Scalar identity must fail before installation: {release}, {namespace}, {strings}, {settings}\n{result.stderr}")
    # Scalar-looking prefixes are still safe string identities.
    check_release(chart, release="true-registry", settings=("metacontroller.fullnameOverride=null-controller",
                                                           "metacontroller.serviceAccount.name=123-controller"),
                  account="123-controller")


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
    check_release(CHART, release="metacontroller-prod", settings=(
        "metacontroller.serviceAccount.name=metacontroller-prod-controller",
    ), account="metacontroller-prod-controller")
    check_release(CHART, settings=("metacontroller.serviceAccount.name=controller-identity",), account="controller-identity")
    check_release(CHART, settings=("metacontroller.serviceAccount.create=false", "metacontroller.serviceAccount.name=existing-controller"), account="existing-controller", create_account=False)
    # Workloads of different kinds may share names when their accounts and selectors differ.
    check_release(CHART, settings=("metacontroller.fullnameOverride=registry",
                                  "metacontroller.serviceAccount.name=controller-identity"), account="controller-identity")
    check_release(CHART, settings=CONTROLLER_SERVICE_SETTINGS, controller_service="registry-metacontroller")
    check_release(CHART, settings=CONTROLLER_SERVICE_SETTINGS + (
        "metacontroller.fullnameOverride=controller-metrics",
    ), account="controller-metrics", controller_service="controller-metrics")
    check_colliding_overrides(CHART)
    check_controller_names(CHART)
    check_service_names(CHART)
    check_controller_labels(CHART)
    check_controller_service_accounts(CHART)
    check_yaml_identity_types(CHART)
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
        check_colliding_overrides(packages[0])
        check_controller_names(packages[0])
        check_service_names(packages[0])
        check_controller_labels(packages[0])
        check_controller_service_accounts(packages[0])
        check_yaml_identity_types(packages[0])
        check_packaged_versions(directory)
    print("Chart verified: strict lint, pinned dependency, bundled CRDs/workload, explicit RBAC, "
          "identity/namespace selectors, collision rejection, webhook connectivity, image digests, values validation, "
          "Kubernetes minimum, PostgreSQL controller/schema contract, offline packaging, "
          "release-version label boundaries, controller pod hostname boundaries, Service name validation, controller label validation, "
          "service-account names, YAML identity types, HTTPS hostname admission, Secret reference names")


if __name__ == "__main__":
    main()
