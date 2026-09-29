# AgentRegistry API reference

API version: `registry.deepai.cloud/v1alpha1` · Kind: `AgentRegistry` · Scope:
namespaced · Short name: `areg`.

The CRD is the machine-readable schema:
[`charts/agentregistry-operator/crds/agentregistries.json`](../charts/agentregistry-operator/crds/agentregistries.json).
The webhook also validates relationships between fields, database URIs, resource
quantities and Secret contents. Use the fields below; arbitrary upstream
application configuration is not exposed.

## Resource identity

`metadata.name` must be a lowercase DNS label beginning with a letter, at most 63
characters. It becomes the name of the generated Service, Deployment,
ServiceAccount and NetworkPolicy in the same namespace. Managed PostgreSQL uses
`<name>-postgres` for its StatefulSet, Service, ConfigMap, Secret and NetworkPolicy;
long names are shortened with a hash to fit the Service name limit. Reserve those
names for the operator. Its StatefulSet creates `data-<database-name>-0` as a PVC.

## Spec

| Field | Required | Default or accepted values |
| --- | --- | --- |
| `version` | Yes | String `"0.3.2"` or `"0.3.3"`; use `"0.3.3"` for a new registry |
| `database` | No | Omit, or omit both host and Secret name, for managed PostgreSQL; immutable after creation |
| `database.storage` | No; managed only | `"1Gi"`; positive integer `Mi` or `Gi` quantity up to 1Pi; choose before creation |
| `database.storageClassName` | No; managed only | Cluster default when omitted; `""` selects no storage class |
| `database.host` | With external `database.secretName` | Namespace-local Service FQDN ending in `.<namespace>.svc.cluster.local`, or a global-unicast literal IP, including private ranges |
| `database.secretName` | With external `database.host` | Same-namespace Secret with key `url`; add `ca.crt` for `verify-full` |
| `authSecretName` | Yes | Same-namespace Secret with `username`, `password`, `jwt-key` |
| `resources.requests.cpu` | No | `"250m"` |
| `resources.requests.memory` | No | `"256Mi"` |
| `resources.limits.cpu` | No | `"1"` |
| `resources.limits.memory` | No | `"1Gi"` |
| `exposure.hostname` | With `exposure` | DNS hostname containing a dot; literal IPs are not allowed |
| `exposure.tlsSecretName` | With `exposure` | Same-namespace Secret with `tls.crt` and `tls.key` |

Omit `database` for operator-managed PostgreSQL. To customize its persistent
volume, set `database.storage` and optionally `database.storageClassName`. To use
your own database instead, set both `database.host` and `database.secretName` and
omit managed storage settings. The entire `database` configuration is immutable,
including whether the field was omitted: choose the mode, size and StorageClass
before creation. Changes require migration or restore into a new registry.

Secret references must be lowercase DNS labels, at most 63 characters, under the
current CRD schema. None of the Secret fields accepts a reference to a different
namespace.

`resources` sizes the upstream `registry` container. The `gateway` container adds
fixed requests of 50m CPU / 32Mi and limits of 250m CPU / 128Mi. CPU accepts positive
whole cores, millicores, or decimals with up to three fractional digits. Memory
accepts positive integer `Mi` or `Gi` quantities. Requests must not exceed their
corresponding limits. Omitted individual fields retain their defaults.

Omit `exposure` for internal HTTP on port 8080. Supply both exposure fields for a
TLS LoadBalancer on 443. `exposure: {}` is invalid. There is no replica-count,
Ingress, custom image, environment-variable or custom-port setting.

## Secrets

| Secret type | Validation |
| --- | --- |
| Authentication | Nonempty username without colon/newline; password at least 16 bytes; JWT key exactly 64 hexadecimal characters |
| Database | `postgres://` or `postgresql://` URI; exact configured host; username, nonempty password and database; port 5432 if specified; no URI fragment |
| Database query | Only one each of `sslmode`, `sslrootcert`, `connect_timeout`; explicit `sslmode=disable`, `require` or `verify-full` |
| Database CA | `verify-full` requires parseable PEM CA data in `ca.crt`, and the URI must set `sslrootcert=/database/ca.crt` |
| Remote database | A literal-IP database requires `verify-full`; the server certificate must cover that IP |
| Public TLS | Matching `tls.crt`/`tls.key`; certificate covers the hostname, allows server authentication and has no unsupported critical extensions |

The webhook validates configuration; database credentials and certificate trust
are exercised by the running application. The gateway's readiness additionally
checks the serving certificate's validity period. TLS certificate issuance,
renewal, public CA trust and DNS remain platform responsibilities.

Managed mode creates an immutable database Secret containing `url`, `username`,
`password`, `postgres-password` and `database`. It reuses those credentials across
reconciliations and operator restarts. The application receives only `url`; the
administrator password remains available only to PostgreSQL. Auth/TLS Secret names
must not collide with the managed database Secret name.

Missing or invalid auth/TLS dependencies keep managed children present and scale
the application Deployment to zero; PostgreSQL remains running. Missing or invalid
managed database credentials also stop PostgreSQL. When a StatefulSet or retained
PVC already exists, the operator refuses to invent replacement credentials;
restore the original database Secret. External database credentials remain
user-managed. Referenced auth/TLS/external database Secret UID/resource-version
changes roll the application. Secret contents are not copied into status.

## Version changes

Application images for 0.3.2 and 0.3.3 are pinned by digest in the operator. The
supported in-place version change is **0.3.2 → 0.3.3**. Same-version configuration
updates are allowed; downgrades are rejected. Version 0.4.x is not supported by
this operator. Back up PostgreSQL before upgrading; the Deployment uses `Recreate`,
so changes interrupt service.

The chart's version, the operator image tag and `spec.version` are distinct:
`spec.version` selects only the upstream registry application. Changing the Helm
image does not change every registry's selected application version.

## Status

| Field | Meaning |
| --- | --- |
| `observedGeneration` | Parent generation processed by the webhook |
| `installedVersion` | Last desired version observed fully Ready; empty before the first successful rollout |
| `endpoints.ui` | Advertised UI URL ending in `/` |
| `endpoints.api` | Advertised API base URL ending in `/v0` |
| `endpoints.mcp` | Advertised MCP endpoint ending in `/mcp` |
| `conditions[]` | Condition type, status, reason, message and observed generation |

| Condition / reason | Meaning |
| --- | --- |
| `DependenciesReady=True`, `Resolved` | Required Secrets exist and their configuration is valid |
| `DependenciesReady=False`, `InvalidOrMissingSecret` | Missing or malformed dependency; message identifies the correction |
| `Ready=False`, `DependencyNotReady` | Application replicas are stopped until dependencies are corrected |
| `Ready=False`, `Progressing` | Waiting for matching children and a healthy current Deployment; managed mode also requires a healthy PostgreSQL StatefulSet |
| `Ready=True`, `Available` | Current children match desired state; the application passes database/MCP readiness and managed PostgreSQL is healthy |

Read `observedGeneration` alongside `metadata.generation` after a change. A
previous `Ready=True` condition can briefly remain visible until reconciliation
processes the new generation. Endpoints advertise desired addresses; they do not
prove external DNS or load-balancer reachability. Invalid specs rejected by the
webhook can leave old status unchanged; inspect Metacontroller logs in that case.

## Managed resources and endpoints

Each registry has one application Deployment (two containers), one application
Service, one ServiceAccount and one application NetworkPolicy. Managed mode adds
a PostgreSQL StatefulSet, headless Service, initialization ConfigMap, immutable
credential Secret and database NetworkPolicy. The StatefulSet provisions a PVC
with retention on deletion and scaling. PostgreSQL runs one replica with fixed
requests of 100m CPU / 128Mi and limits of 1 CPU / 512Mi; it creates the `registry`
database, dedicated role, and `pg_trgm`/`vector` extensions. The managed
database connection uses `sslmode=disable` inside the namespace network boundary;
use external mode when encrypted database transport is required.

The application listens only on loopback inside the pod; client traffic passes
through the authenticated gateway. The operator does not create namespaces,
auth/TLS Secrets, DNS records or certificates, and does not back up database data.

| Route | Purpose |
| --- | --- |
| `/` | Upstream UI |
| `/v0` | API base; catalog resources are servers, agents, skills and prompts, with health/version routes |
| `/mcp` | MCP catalog interface; execution tools are blocked |

All client routes require Basic authentication. Registry artifact storage is
supported; running agents, deploying workloads and executing tools are outside
the operator's scope. Some upstream UI execution controls may remain visible but
are denied by the gateway.
