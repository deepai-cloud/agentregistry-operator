# AgentRegistry operator Helm chart

Install the operator and its **bundled Metacontroller 4.17.2** in one Helm release.
The chart includes the AgentRegistry and Metacontroller CRDs, webhook Deployment
and Service, service accounts, explicit controller RBAC, a webhook NetworkPolicy,
and the CompositeController that manages registry instances. All namespaced
control-plane resources share the Helm release namespace.

Use **one release per cluster**. Kubernetes 1.30 or newer and Helm 3 are required.
A cluster administrator must install the cluster-scoped CRDs and RBAC. A CNI that
enforces NetworkPolicy is required for the documented isolation guarantees.

## Install

Select a published version from the
[repository releases](https://github.com/deepai-cloud/agentregistry-operator/releases).
The release workflow publishes both the operator/gateway image and the OCI chart;
installation does not require building an image or cloning the repository.

```sh
export OPERATOR_VERSION=0.1.0 # replace with the published version you want
helm upgrade --install agentregistry-operator \
  oci://ghcr.io/deepai-cloud/charts/agentregistry-operator \
  --version "$OPERATOR_VERSION" --kube-context "$KUBE_CONTEXT" \
  --namespace agentregistry-system --create-namespace \
  --wait --timeout 5m
kubectl --context "$KUBE_CONTEXT" get pods -n agentregistry-system
```

The published chart selects its matching image at
`ghcr.io/deepai-cloud/agentregistry-operator:v<version>`. The packages must be
published and readable by your cluster; see the
[release guide](../../docs/releases.md) for initial publication and visibility.
Set `image.digest` to the image's `sha256:...` digest for an immutable pin;
a nonempty digest overrides the tag. The same reference is passed to managed
gateway containers.

For local development, build and load `agentregistry-operator:dev` into every
node, then install from a checkout with explicit local image overrides:

```sh
helm upgrade --install agentregistry-operator ./charts/agentregistry-operator \
  --namespace agentregistry-system --create-namespace \
  --set-string image.repository=agentregistry-operator --set-string image.tag=dev \
  --wait --timeout 5m
```

Metacontroller is already vendored under `charts/`; a separate Metacontroller
installation or dependency download is unnecessary. If another Metacontroller
already runs in the cluster, plan consolidation before installing: multiple
controllers watching the same resources can compete. This distribution supports
one dedicated, bundled Metacontroller and does not support an external-controller
mode.

Continue with the [local quickstart](../../docs/quickstart.md) for a complete
working registry, or the [registry creation guide](../../docs/creating-a-registry.md)
for an existing database. The [installation guide](../../docs/installation.md)
covers published packages and installing on an existing cluster.

## Values

| Value | Default | Purpose |
| --- | --- | --- |
| `image.repository` | `ghcr.io/deepai-cloud/agentregistry-operator` | Repository for the operator and managed gateway image |
| `image.tag` | `v0.1.0` | Image tag; quote numeric-looking tags in values files |
| `image.digest` | `""` | Optional immutable `sha256:...` digest; overrides the tag |
| `image.pullPolicy` | `IfNotPresent` | Webhook image pull policy |
| `replicaCount` | `1` | Stateless webhook replicas |
| `resources.requests` | CPU `50m`, memory `64Mi` | Webhook resource requests |
| `resources.limits` | CPU `500m`, memory `128Mi` | Webhook resource limits |
| `podAnnotations` | `{}` | Additional webhook pod annotations |
| `nodeSelector` | `{}` | Webhook node selection |
| `tolerations` | `[]` | Webhook tolerations |
| `affinity` | `{}` | Webhook scheduling affinity |
| `metacontroller.rbac.create` | `false` | Must stay false; explicit permissions are supplied here |
| `metacontroller.replicas` | `1` | Singleton controller; this chart does not grant leader-election permissions |
| `metacontroller.image.tag` | `v4.17.2` | Tested bundled controller image |
| `metacontroller.nameOverride` | `""` | Override the dependency's application name label |
| `metacontroller.fullnameOverride` | `""` | Override the dependency's resource name |
| `metacontroller.namespaceOverride` | `""` | Must stay empty; both controllers use the Helm release namespace |
| `metacontroller.serviceAccount.create` | `true` | Create the dependency's service account |
| `metacontroller.serviceAccount.name` | `""` | Generated name; an explicit name is required when creation is disabled |

Additional upstream dependency settings, such as `metacontroller.resources`,
`metacontroller.nodeSelector`, and `metacontroller.tolerations`, are available under
`metacontroller`. Inspect them with:

```sh
helm show values ./charts/agentregistry-operator/charts/metacontroller-helm-4.17.2.tgz
```

Webhook scheduling and resources do not configure managed registries. Registry
resources are configured through each AgentRegistry's `spec.resources`. Gateway
containers use `IfNotPresent` independently of the webhook pull policy. The chart
does not distribute private image pull credentials to registry namespaces; use
an image accessible to all nodes that will run registry pods. The current
controller assumes the cluster DNS suffix `cluster.local`.

Cross-namespace Metacontroller deployment is rejected by values validation. The
webhook NetworkPolicy permits TCP 8080 only from the bundled controller's pods in
the same namespace and denies webhook egress. Resource-name and service-account
overrides remain supported.

## Verify and troubleshoot

```sh
helm status agentregistry-operator -n agentregistry-system
kubectl rollout status deployment/agentregistry-operator -n agentregistry-system
kubectl get pods -n agentregistry-system \
  -l app.kubernetes.io/instance=agentregistry-operator
kubectl logs -n agentregistry-system deployment/agentregistry-operator
kubectl get agentregistries --all-namespaces
```

`--wait` verifies the controller workloads. Each registry has its own readiness
condition; check it separately after creating its authentication Secret and
AgentRegistry. Managed PostgreSQL and its database are provisioned automatically;
external mode also requires the referenced database Secret.
For `ImagePullBackOff`, inspect pod events and confirm that the operator image was
published or loaded into the cluster. If a registry reports
`DependencyNotReady`, inspect its conditions and referenced Secrets.

## Upgrades and uninstall

Helm installs CRDs on first installation, but **does not upgrade or remove them**.
Review CRD changes in each version and apply schema upgrades as a separate
platform operation before upgrading the release. Back up AgentRegistry resources
and database data before changes that affect their schema or application version.

Before deleting a managed registry, back up both its database and the generated
database Secret. Deleting the AgentRegistry removes its PostgreSQL StatefulSet,
generated Secret and initialization ConfigMap alongside its registry Deployment,
Services, ServiceAccount and NetworkPolicies. Its database PVC is retained.
When recreating a registry over retained storage, restore the original generated
Secret and set its `controller-uid` label to the recreated parent's UID, as shown
in the [recovery procedure](../../docs/operations.md#delete-a-registry). The operator
refuses to generate replacement credentials when its PVC or StatefulSet already
exists. External databases and user-created Secrets remain externally managed.

Before uninstalling, delete tenant AgentRegistry resources while the controllers
are still running and verify their managed children have disappeared. Preserve
retained PVCs until their data is backed up or deliberately retired. Remove the
release only after that cleanup:

```sh
helm uninstall agentregistry-operator --namespace agentregistry-system
```

CRDs and namespaces remain. Deleting a CRD also deletes every resource of that
type, so it is a separate administrative decision.

## Permissions and ownership

Metacontroller owns application children; Helm owns the webhook, controller
configuration, and bundled dependency. The webhook has no Kubernetes API token
and accepts traffic only from the bundled Metacontroller when NetworkPolicy is
enforced.

Metacontroller uses shared informers and requires cluster-wide reads, including
Secrets. Related-resource selection limits webhook payloads, not cache access. The
CRDs and CompositeController are cluster-scoped, so installing this chart with
only a namespaced Role is unsupported. The explicit RBAC grants child-resource
writes and parent status/finalizer updates;
it intentionally disables upstream wildcard and aggregated RBAC. Finalizer
permission enables Kubernetes owner-reference admission with
`blockOwnerDeletion`; it does not install a cleanup hook. Read the repository's
[installation and operating guidance](../../docs/installation.md) before granting tenant write access.
