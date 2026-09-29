# Install the operator

The Helm chart installs the operator webhook, AgentRegistry CRD, a
CompositeController, and the bundled Metacontroller dependency in one release.
Install one operator release per cluster. An individual release manages registries
across tenant namespaces.

These instructions install a published release on an existing cluster without
building an image or cloning the repository. Source is available at
[deepai-cloud/agentregistry-operator](https://github.com/deepai-cloud/agentregistry-operator).
For a disposable local installation from source, use the [quickstart](quickstart.md).

## Requirements

- A Kubernetes cluster satisfying the chart's `kubeVersion` constraint, using the
  `cluster.local` DNS domain and a CNI that enforces ingress and egress NetworkPolicy.
- Helm 3 and kubectl, with permission to install CRDs and cluster-scoped RBAC.
- Access to GHCR from Helm and every node that can run the webhook or tenant
  registry pods. Published images support Linux amd64 and arm64.
- A default StorageClass for each registry's managed PostgreSQL PVC, or an explicit
  `database.storageClassName`/pre-provisioned volume. Alternatively, supply an
  external PostgreSQL database with `pg_trgm`, `vector`, credentials and network
  access. PostgreSQL is provisioned per registry by the operator, not by Helm.
- For public HTTPS: a LoadBalancer implementation, DNS and a valid certificate.

The webhook and Metacontroller are cluster administration infrastructure. Their
Secret access is described in [security and tenancy](security.md). If a cluster
already runs Metacontroller or another AgentRegistry operator, review that
installation before adding this release; this chart is designed to own its bundled
Metacontroller.

## 1. Choose a published release

Choose an available version from the repository's
[releases](https://github.com/deepai-cloud/agentregistry-operator/releases). The
[release workflow](../.github/workflows/release.yaml) builds and publishes these
artifacts when a maintainer pushes a version tag:

| Artifact | Location |
| --- | --- |
| Operator and gateway image | `ghcr.io/deepai-cloud/agentregistry-operator:v<VERSION>` |
| Helm chart | `oci://ghcr.io/deepai-cloud/charts/agentregistry-operator`, version `<VERSION>` |
| Chart archive, source bundle and checksums | GitHub release assets |

The workflow must have completed for the selected version. Before the first
login-free installation, a maintainer must make both GHCR packages public and
verify anonymous pulls; see [publishing a version](releases.md#publish-a-version).
The existence of this source checkout does not imply that a release is published.

The versioned chart already selects its matching operator image, shared by the
webhook and each tenant's authentication gateway. The chart does not configure
tenant `imagePullSecrets`; a private mirror requires platform-managed image access
in every registry namespace. To pin an immutable image, set `image.digest` to the
published manifest's `sha256:...` digest; it takes precedence over the image tag.

## 2. Select a context and install the chart

Set `OPERATOR_VERSION` to the selected release version without the leading `v`,
for example `0.1.0` once that release has been published. Set the context
deliberately, then review the destination:

```sh
export OPERATOR_VERSION='<VERSION>'
export KUBE_CONTEXT=your-cluster-context
kubectl --context "$KUBE_CONTEXT" cluster-info

helm upgrade --install agentregistry-operator \
  oci://ghcr.io/deepai-cloud/charts/agentregistry-operator \
  --version "$OPERATOR_VERSION" \
  --kube-context "$KUBE_CONTEXT" \
  --namespace agentregistry-system --create-namespace \
  --wait --timeout 5m
```

The tested Metacontroller 4.17.2 chart is vendored in
`charts/agentregistry-operator/charts/` with a verified archive checksum; its runtime
image uses the pinned version tag `v4.17.2`.
An initial install does not need `helm dependency update` or a separate
Metacontroller release. Its default wildcard RBAC is disabled; this chart grants
explicit permissions for the watched and managed resources.

Review [chart values](../charts/agentregistry-operator/README.md) for image,
resource and Metacontroller settings. Store your chosen settings in a versioned
values file without credentials so later upgrades use the same configuration.

## Namespace and permission scope

The webhook and bundled Metacontroller run in the Helm release namespace,
`agentregistry-system` above. `metacontroller.namespaceOverride` must remain empty;
the chart rejects overrides that could separate the controllers. Each registry's
managed workloads, Secrets and policies stay in its AgentRegistry namespace.

This is not a namespace-only installation: CRDs, the CompositeController and
controller RBAC are cluster-scoped. Metacontroller's shared informers require
cluster-wide reads, including Secrets, and the release manages registries across
tenant namespaces. Namespaced RBAC alone cannot support this controller. Install
it as trusted cluster administration infrastructure, even when all registries
are in one namespace.

The chart disables upstream wildcard RBAC and grants explicit resource kinds and
verbs. The webhook has no Kubernetes API token; its NetworkPolicy accepts TCP 8080
only from the bundled Metacontroller in the same namespace and denies webhook
egress. See [security and tenancy](security.md) for tenant network boundaries and
the remaining administrative trust requirements.

## 3. Verify the installation

```sh
helm --kube-context "$KUBE_CONTEXT" -n agentregistry-system status agentregistry-operator
kubectl --context "$KUBE_CONTEXT" -n agentregistry-system get pods
kubectl --context "$KUBE_CONTEXT" get crd agentregistries.registry.deepai.cloud
kubectl --context "$KUBE_CONTEXT" get compositecontroller agentregistry-operator
```

All operator and Metacontroller pods should be Ready. The Helm wait checks the
control plane; it does not create a registry or verify your database.
Continue with [creating a registry](creating-a-registry.md).

## Optional source checkout

To inspect the chart or use the local development quickstart:

```sh
git clone git@github.com:deepai-cloud/agentregistry-operator.git
cd agentregistry-operator
```

The published chart installation above does not require a checkout. Local builds
and release maintenance are covered in [packaging and releases](releases.md).

## Upgrade and uninstall

Use the [operations guide](operations.md) for the two distinct upgrade paths:
operator/chart upgrades and individual registry application upgrades. Helm
retains CRDs and does not upgrade files in `crds/` automatically, so schema changes
need an explicit step before a chart upgrade.

Before uninstalling the operator, delete the intended AgentRegistry resources and
verify that their children disappear while the controller is still running.
Managed PostgreSQL resources, including their generated Secrets, are deleted;
database PVCs are retained. Back up both database contents and the generated
database Secret before deletion. External databases, supplied authentication/TLS
Secrets, namespaces and backups remain independently managed. Avoid
removing the CRD as a routine uninstall step; doing so deletes every AgentRegistry
object in the cluster.
