# AgentRegistry Operator

Run an authenticated [AgentRegistry](https://github.com/agentregistry-dev/agentregistry)
catalog on Kubernetes with one `AgentRegistry` resource per namespace-local
installation. The operator provisions and repairs the registry Deployment,
Service, ServiceAccount and NetworkPolicy. By default it also provisions PostgreSQL
with persistent storage; you supply the registry authentication Secret.

**One Helm release installs the operator, CRDs and bundled Metacontroller.**
Each registry exposes its UI, API and MCP catalog through one authenticated
endpoint. Private HTTP and public HTTPS configurations are supported.

## Try it locally

With Docker running, k3d, kubectl, Helm 3, Python 3 and Make installed:

```sh
make demo-up
```

This creates a dedicated local K3s cluster, builds and loads the operator image,
installs the chart, provisions PostgreSQL with a persistent volume, generates
credentials and waits for `tenant-a/catalog` to become Ready. It uses a separate
kubeconfig and preserves your current Kubernetes context.

Start a port-forward:

```sh
kubectl --kubeconfig .local/quickstart/kubeconfig -n tenant-a \
  port-forward --address=127.0.0.1 service/catalog 18080:8080
```

Open **http://127.0.0.1:18080/** and use the generated credentials in
`.local/quickstart/credentials.txt`. Delete the demo and its database with
`make demo-down`. Deleting the demo cluster destroys its database volume.

See the [step-by-step quickstart](docs/quickstart.md) for prerequisites, API access
and cleanup, or [install on an existing cluster](docs/installation.md).

## Install with Helm

Choose a published version from
[GitHub releases](https://github.com/deepai-cloud/agentregistry-operator/releases)
and install its OCI chart. The chart selects the matching GHCR operator/gateway
image, so installation needs no source checkout or local build.

```sh
export OPERATOR_VERSION='<VERSION>' # Release version without the leading v
export KUBE_CONTEXT=your-cluster-context

helm upgrade --install agentregistry-operator \
  oci://ghcr.io/deepai-cloud/charts/agentregistry-operator \
  --version "$OPERATOR_VERSION" \
  --kube-context "$KUBE_CONTEXT" \
  --namespace agentregistry-system --create-namespace \
  --wait --timeout 5m
```

The [installation guide](docs/installation.md) covers release availability, GHCR
access and verification. Before first use, a maintainer must publish a version and
enable public access to both GHCR packages. Install one operator release per
cluster. Kubernetes 1.30+, `cluster.local` DNS and enforced ingress/egress
NetworkPolicies are required. Metacontroller runs alongside the webhook in the
same namespace; CRDs and controller permissions remain cluster-scoped. Managed PostgreSQL
needs a default StorageClass, or an explicit storage configuration. [Chart values](charts/agentregistry-operator/README.md) cover image
digests, resources and scheduling.

## Create a registry

After preparing a tenant namespace and an authentication Secret:

```yaml
apiVersion: registry.deepai.cloud/v1alpha1
kind: AgentRegistry
metadata:
  name: catalog
  namespace: tenant-a
spec:
  version: "0.3.3"
  authSecretName: registry-auth
```

```sh
kubectl --context "$KUBE_CONTEXT" apply -f examples/private.yaml
kubectl --context "$KUBE_CONTEXT" -n tenant-a \
  wait --for=condition=Ready agentregistry/catalog --timeout=5m
```

The [registry creation guide](docs/creating-a-registry.md) explains database
provisioning, Secret keys, storage and network policies. Copy a complete example:

| Example | Use it for |
| --- | --- |
| [Private registry](examples/private.yaml) | Namespace-local UI, API and MCP access |
| [HTTPS registry](examples/tls.yaml) | A certificate, hostname and LoadBalancer endpoint |
| [Remote database](examples/remote-database.yaml) | An external PostgreSQL IP with verified TLS |
| [External database](examples/external-database.yaml) | An independently managed PostgreSQL Service |

## Documentation

- [Installation](docs/installation.md) and [local quickstart](docs/quickstart.md)
- [Creating registries](docs/creating-a-registry.md) and [examples](examples/README.md)
- [API reference](docs/api-reference.md)
- [Upgrades, cleanup and troubleshooting](docs/operations.md)
- [Security and tenancy](docs/security.md)
- [Packaging and releases](docs/releases.md)

## Scope and lifecycle

Supported registry application versions are **0.3.2 and 0.3.3**. The operator
manages catalog installations; the gateway blocks execution APIs. Registries run
one pod and use `Recreate` rollouts, so changes briefly interrupt service.

Deleting a registry removes its managed workloads and generated database Secret;
it retains database PVCs, external databases, user-created Secrets and backups.
Back up generated database credentials before deletion if you need to reuse the
volume. Missing or invalid Secrets stop the application until
repaired. Secret rotation triggers a rollout. Public endpoints need platform-managed
DNS, certificates and LoadBalancer capacity.

Metacontroller has cluster-wide Secret read access and must be administered as
trusted platform infrastructure. See the [security model](docs/security.md).

## Development

Go 1.25.4, Helm 3 and Python 3 are needed for local verification. Docker and k3d
are needed for the demo.

```sh
make help       # available commands
make verify     # race tests, vet, builds, chart contracts and documentation links
make package    # Helm chart, source/examples bundle and SHA256SUMS in dist/
make image      # combined operator/gateway container
```

Packaging is local. The [release workflow](.github/workflows/release.yaml) publishes
versioned multi-platform images, an OCI Helm chart and downloadable bundles when
a maintainer pushes a release tag. Nothing is published by `make package`.

## License

AgentRegistry Operator is licensed under the [MIT License](LICENSE).
Vendored Metacontroller retains its [Apache 2.0 license](charts/agentregistry-operator/METACONTROLLER-LICENSE).
