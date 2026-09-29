# Create a registry

An `AgentRegistry` creates an authenticated catalog and, by default, PostgreSQL
with persistent storage. You supply an authentication Secret and a namespace;
for HTTPS, also supply a certificate, load-balancer capacity and DNS. Install the
[operator](installation.md) first.

The commands below use `tenant-a/catalog`. Keep `KUBE_CONTEXT` set to the intended
cluster and run commands from the repository root. The [quickstart](quickstart.md)
performs these steps in a disposable local cluster.

## 1. Prepare the namespace and authentication

```sh
kubectl --context "$KUBE_CONTEXT" apply -f examples/namespace-baseline.yaml
```

Create the namespace-local `registry-auth` Secret through your secret manager:

| Key | Content |
| --- | --- |
| `username` | Nonempty Basic-auth username without `:`, CR or LF |
| `password` | At least 16 bytes; use a randomly generated password |
| `jwt-key` | Exactly 64 hexadecimal characters, generated randomly |

For example, create the Secret from files prepared in a private directory outside
version control. Each file must contain its value without an unwanted trailing
newline:

```sh
kubectl --context "$KUBE_CONTEXT" -n tenant-a create secret generic registry-auth \
  --from-file=username=/secure/registry/username \
  --from-file=password=/secure/registry/password \
  --from-file=jwt-key=/secure/registry/jwt-key
```

For local learning, `python3 examples/local/create-secrets.py` generates a random
authentication Secret manifest and credential files in `.local/quickstart/`; it
does not contact a cluster. Apply its `secrets.json` after creating the namespace.

## 2. Choose database storage and create the registry

The [minimal example](../examples/private.yaml) uses managed PostgreSQL and a 1Gi
volume from the default StorageClass:

```sh
kubectl --context "$KUBE_CONTEXT" apply -f examples/private.yaml
kubectl --context "$KUBE_CONTEXT" -n tenant-a \
  wait --for=condition=Ready agentregistry/catalog --timeout=5m
kubectl --context "$KUBE_CONTEXT" -n tenant-a get areg catalog
kubectl --context "$KUBE_CONTEXT" -n tenant-a get statefulset,pvc
```

The operator creates a dedicated PostgreSQL StatefulSet, database credentials,
`pg_trgm` and `vector` extensions, initialization ConfigMap, internal Service,
NetworkPolicy and PVC. The registry's application role is not a PostgreSQL
superuser. Managed database connections use namespace-local plaintext PostgreSQL
inside the network-policy boundary; use an [external TLS database](#use-an-external-database)
when database transport encryption is required.

For a new installation, customize storage before applying the resource:

```yaml
spec:
  version: "0.3.3"
  authSecretName: registry-auth
  database:
    storage: "10Gi"
    storageClassName: your-storage-class
```

Omit `storageClassName` to use the default StorageClass. An empty string explicitly
selects no StorageClass and requires a suitable statically provisioned volume.
Storage must be a positive integer quantity in `Mi` or `Gi`. Database mode,
storage size and StorageClass are immutable after creation. Plan capacity before
creating the registry; changes require a migration or restore into a new one.

For `catalog`, the database is named `catalog-postgres`, and its PVC is
`data-catalog-postgres-0`. Long registry names are shortened with a hash to fit the
Kubernetes name limit. The operator retains the PVC when the registry is deleted,
but the generated database Secret is deleted with the managed children. Back up
both database data and the Secret before deleting a recoverable installation.

## 3. Access the private registry

`Ready=True` means the current application resources and managed database are
healthy, including a real PostgreSQL query and MCP initialization. Its endpoints
are:

| Interface | URL |
| --- | --- |
| UI | `http://catalog.tenant-a.svc.cluster.local:8080/` |
| API | `http://catalog.tenant-a.svc.cluster.local:8080/v0` |
| MCP | `http://catalog.tenant-a.svc.cluster.local:8080/mcp` |

All require the configured Basic-auth credentials. With NetworkPolicy enforced,
the private Service accepts traffic only from pods in the same namespace. Use
`kubectl port-forward` for local administrative access as shown in the quickstart.
HTTP Basic authentication does not encrypt traffic; enable HTTPS when needed.

## Enable HTTPS

Choose a hostname and provide a trusted certificate covering it with its private
key. The certificate must permit server authentication, be within its validity
period and have no unsupported critical extensions. Include the certificate chain
in `tls.crt` as required by your clients:

```sh
kubectl --context "$KUBE_CONTEXT" -n tenant-a create secret tls registry-tls \
  --cert=/secure/registry/fullchain.pem --key=/secure/registry/tls.key
```

For a new registry, copy [`examples/tls.yaml`](../examples/tls.yaml) and adjust the
hostname and resource sizing before applying. Add storage settings before
creation if the default 1Gi is insufficient. To expose an existing
registry, add only `exposure` without changing its immutable database settings:

```sh
kubectl --context "$KUBE_CONTEXT" -n tenant-a patch agentregistry catalog \
  --type=merge -p '{"spec":{"exposure":{"hostname":"catalog.example.com","tlsSecretName":"registry-tls"}}}'
kubectl --context "$KUBE_CONTEXT" -n tenant-a get service catalog -w
```

Replace `catalog.example.com` with your hostname before running the patch. The
Service switches to a LoadBalancer on 443. Configure an A/AAAA or CNAME record
pointing to its assigned address. The operator does not create DNS records, issue
certificates or create an Ingress. Ensure the platform can expose port 443;
bundled ingress controllers can already occupy it on small K3s installations.

Verify HTTPS from outside the cluster, including certificate trust and
authentication. A request without credentials should receive HTTP 401; an
authenticated `/v0/servers` request should succeed. Registry readiness does not
prove external routing, DNS propagation or the client's certificate trust.

## Use an external database

To use your own PostgreSQL, create a new registry with both `database.host` and
`database.secretName`, as in [`examples/external-database.yaml`](../examples/external-database.yaml).
The operator then manages only application resources and leaves the database and
its credentials untouched. You cannot switch an existing registry between managed
and external modes.

Use an independent database and role for each registry. The application role needs
permission to own and migrate its schema, but not superuser, role-creation or
database-creation privileges. Have the administrator preinstall both extensions
in the target database:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS vector;
```

The pinned application versions may reference pgvector during migration even
though vector search and embeddings are disabled. PostgreSQL must have the
extension software installed; a plain PostgreSQL image does not provide pgvector.
The optional [local PostgreSQL example](../examples/local/postgres.yaml) supplies
a disposable external database using `emptyDir`; it is not needed for managed mode.

For a database inside the registry's namespace, use the full Service name such as
`postgres.tenant-a.svc.cluster.local`. Label its pods
`registry.deepai.cloud/database: catalog` so the registry's egress policy can
select them, and permit TCP 5432 in the database's ingress policy. The namespace
baseline alone denies database ingress.

Supply a namespace-local Secret with `url` and, for `verify-full`, `ca.crt`:

```text
postgres://REGISTRY_USER:ENCODED_PASSWORD@postgres.tenant-a.svc.cluster.local:5432/registry?sslmode=verify-full&sslrootcert=/database/ca.crt
```

Replace placeholders and URI-encode special characters in credentials. The host
must exactly match `spec.database.host`. Port 5432 may be explicit or omitted.
Only `sslmode`, `sslrootcert` and `connect_timeout` query keys are supported, each
at most once. Modes are `disable`, `require` and `verify-full`; prefer
`verify-full` to verify the server identity. `require` encrypts without identity
verification; reserve `disable` for a trusted local network path.

```sh
kubectl --context "$KUBE_CONTEXT" -n tenant-a create secret generic registry-database \
  --from-file=url=/secure/registry/database-url \
  --from-file=ca.crt=/secure/registry/database-ca.crt
```

Omit the `ca.crt` file only when your TLS mode does not require it. Cross-namespace
Services, `ExternalName` indirection and external DNS database names are not
supported. Use a literal IP for a remote database.

## Use a remote database

Copy [`examples/remote-database.yaml`](../examples/remote-database.yaml) and replace
its documentation-only address with a reachable literal IPv4 or IPv6 address.
Private addresses are allowed; loopback, multicast and link-local addresses are
rejected.

Use `sslmode=verify-full&sslrootcert=/database/ca.crt` in the Secret's URI, supply
the signing CA in `ca.crt`, and ensure the database certificate has a matching
**IP Subject Alternative Name**. A DNS SAN does not cover an IP. For IPv6, bracket
the address in the URI but not in `spec.database.host`.

The operator permits egress only to that IP on TCP 5432, plus cluster DNS. Your
platform supplies routing, firewall rules and PostgreSQL authorization. Managed
databases reachable only through a changing DNS name do not fit this external API.

## Create another registry

Choose a new registry name and preferably a separate tenant namespace. Supply a
new authentication Secret and optional hostname. Each managed registry gets its
own PostgreSQL instance, credentials and volume. For external mode, also provide
an independent database, role and correctly labeled database pods.
