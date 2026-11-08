# Operate and troubleshoot

Use the explicit intended `KUBE_CONTEXT` in these examples. For the local demo,
replace `--context "$KUBE_CONTEXT"` with
`--kubeconfig .local/quickstart/kubeconfig`.

## Observe a registry

```sh
kubectl --context "$KUBE_CONTEXT" -n tenant-a get areg catalog
kubectl --context "$KUBE_CONTEXT" -n tenant-a describe areg catalog
kubectl --context "$KUBE_CONTEXT" -n tenant-a get deployment/catalog -o wide
kubectl --context "$KUBE_CONTEXT" -n tenant-a get events --sort-by=.metadata.creationTimestamp
```

`Ready=True` checks current desired children, a healthy one-pod application
Deployment, an actual registry database query and MCP initialization. Managed
mode also requires a healthy current PostgreSQL StatefulSet. A database outage
makes readiness fail. The public health API alone is not sufficient evidence of
a working database. For HTTPS, verify DNS, load-balancer routing, trust and Basic
authentication externally as well.

Application and managed PostgreSQL Service selectors must exactly match the
operator's desired labels. An additional selector label can exclude all pods even
when the workloads are healthy; readiness remains `Progressing` until that label
is removed. API-defaulted Service fields and unrelated annotations remain allowed.

The operator's application and PostgreSQL NetworkPolicy specs must also match
the desired policies. Extra pod selector labels, namespace selectors, port ranges
or egress rules can change which traffic is allowed. Such drift keeps
`Ready=False` with reason `Progressing` and prevents `installedVersion` from
advancing. Unrelated annotations remain allowed. This status check does not scale
down running workloads; restore the desired policy and remove unexpected fields
to recover readiness.

The registry container also checks its own loopback `/v0/health` endpoint using
the `curl` included in both supported application images. Startup allows up to
ten minutes for database initialization and migrations. After startup, three
consecutive failed checks at ten-second intervals cause Kubernetes to restart
the registry container. Each request has a two-second timeout. This static health
endpoint does not query PostgreSQL; database outages remove readiness without
triggering application restarts while the HTTP server remains responsive.

The MCP readiness probe requires one `Content-Type` header on the initialization
reply, either `application/json` or `text/event-stream`. Media type names are
case-insensitive and can include parameters such as `charset=utf-8`. A missing,
malformed or unsupported type makes `/readyz` return HTTP 503.
On an SSE stream the probe skips empty events, custom events such as keepalives,
and intermediate JSON-RPC notifications. A matching reply in an unnamed or
explicit `message` event completes the probe without waiting for the stream to
close. The probe closes its response stream and attempts to delete its temporary
MCP session after success or failure, using the negotiated protocol version
when initialization succeeds.

For a new managed database, the operator first creates its immutable credentials
Secret. Until that Secret is observed, `DependenciesReady=False` reports
`Provisioning`, the application stays at zero replicas, and no PostgreSQL
StatefulSet is created. Secret-creation failures can then retry without creating
storage with missing credentials. Once the Secret is observed, provisioning
continues automatically. If the Secret is already marked for deletion, a fresh
installation waits for deletion to finish before generating credentials and
retrying. If a StatefulSet or retained PVC already exists, the operator requests
zero replicas for both workloads until the original Secret is restored.

Managed PostgreSQL also resumes interrupted registry initialization. Until role,
database and extension setup completes, it listens only on a local Unix socket;
the TCP readiness probe remains unsuccessful. A restart retries missing steps
without resetting an existing role password or replacing database contents.
A completion marker in `PGDATA` keeps later restarts on the ordinary PostgreSQL
startup path. Existing volumes without that marker receive the same idempotent
setup once. Bootstrap script changes trigger a database pod rollout.

The PostgreSQL pod mounts only `start-postgres` and `01-registry.sh` from its
bootstrap ConfigMap. Extra keys cannot introduce SQL or shell hooks during
initialization. Upgrading from a version that mounted the whole ConfigMap
restarts the managed database pod once; its existing PVC and credentials are reused.

This recovery covers interrupted registry setup after PostgreSQL has initialized
its data directory. It does not repair corrupt storage or substitute for restoring
the original Secret and database backup.

## Rotate credentials and certificates

Update authentication and TLS Secrets through your secret manager. Keep all
required keys and the same Secret name. The controller rolls out the registry when
Secret data changes, including external database credentials and CA certificates.
Updates only to Secret labels or annotations do not trigger a
rollout. Rollouts use `Recreate`, causing a service interruption.

The gateway rereads authentication files for each request. For Kubernetes Secret
mounts, it reads username and password from one projected revision so a rotation
cannot combine the old username with the new password. If the revision is removed
during the read, that request returns HTTP 503; subsequent requests read the
current revision. Ordinary credential directories without a `..data` link remain
supported.

TLS handshakes and readiness checks also read the certificate and private key
from one projected Secret revision. If that revision disappears during the read,
the gateway retries once against the new revision. It does not cache an old
certificate when current material is invalid. A second rotation during the retry
can still fail that handshake or readiness check; the next attempt rereads the
current revision. Ordinary certificate/key files and custom symlinks remain
supported.

Managed database Secrets are immutable; in-place database password rotation is
not supported. Back up their original contents, and restore the original Secret
if it is lost. Do not delete it to trigger credential regeneration: the operator
refuses to generate new passwords when it detects the existing StatefulSet or PVC.
The restored `password` and `postgres-password` must be distinct and free of NUL
bytes so the pod can receive both values intact. The `url` must begin with lowercase
`postgres://` or `postgresql://`, have no fragment and contain the registry role's
matching password. If these checks fail, the operator requests zero replicas for
both workloads.
Restore the original Secret from backup to resume them.

For an external database, arrange for new credentials to be valid before updating
the URI Secret. Changing a Kubernetes Secret alone does not change PostgreSQL
passwords. Keep issuer chains and hostnames correct when
rotating the TLS Secret. As soon as the operator observes an auth, TLS or external
database Secret marked for deletion, it requests zero registry replicas and names
the terminating Secret in `DependenciesReady`. A finalizer can keep the Secret
visible in this state. Once deletion finishes, provide a replacement with the
same name. The registry stays at zero replicas until the required Secret data
is valid. Managed PostgreSQL keeps running when only auth or TLS is unavailable.

Do not edit managed Deployments or StatefulSets to rotate credentials or tune
resources; the operator restores desired state. Update supported Secrets or the
AgentRegistry spec. Database configuration, including mode, connection reference,
volume size and storage class, is immutable after creation; use a new installation
for database migration or a different storage configuration.

## Upgrade a registry application

Back up the database and confirm you can restore it before changing the version.
For a registry currently on 0.3.2:

```sh
kubectl --context "$KUBE_CONTEXT" -n tenant-a patch agentregistry catalog \
  --type=merge -p '{"spec":{"version":"0.3.3"}}'
kubectl --context "$KUBE_CONTEXT" -n tenant-a get areg catalog -w
```

Wait for `status.observedGeneration` to match the resource generation,
`status.installedVersion` to show `0.3.3`, and the current `Ready` condition to be
true. Immediately running `kubectl wait` after an update may observe the previous
generation's condition before the controller processes the change.

Only the 0.3.2 → 0.3.3 upgrade is supported. There is no in-place downgrade. Restore
a compatible backup into a separate database and create a new registry when a
rollback requires an older database schema. Application 0.4.x is not accepted.

## Upgrade the operator and chart

Select a published chart version from the
[releases](https://github.com/deepai-cloud/agentregistry-operator/releases) and retain
your chosen Helm values. Pull the chart to review schema changes and apply the
AgentRegistry CRD before upgrading, because Helm does not update resources from
`crds/` on upgrade. Use a fresh working directory for the extracted chart:

```sh
export OPERATOR_VERSION=0.1.0 # replace with the desired published version
helm pull oci://ghcr.io/deepai-cloud/charts/agentregistry-operator \
  --version "$OPERATOR_VERSION" --untar
kubectl --context "$KUBE_CONTEXT" apply \
  -f agentregistry-operator/crds/agentregistries.json
helm upgrade agentregistry-operator ./agentregistry-operator \
  --kube-context "$KUBE_CONTEXT" --namespace agentregistry-system \
  -f /path/to/operator-values.yaml --wait --timeout 5m
```

The chart selects the matching published image. Remove stale image overrides from
your values file, or update an explicit image tag or digest to the intended version.
If upgrading the bundled Metacontroller dependency, review and apply its required
CRD changes too. Keep `metacontroller.namespaceOverride` empty; both controllers
must run in the release namespace.
Changing the operator image also changes the gateway image desired for each
registry and therefore triggers tenant rollouts. Plan for those interruptions.

## Recover a registry with a long name

Earlier builds generated database names that left insufficient room for the
StatefulSet pod's `-0` suffix when the AgentRegistry name was 53–63 characters.
Kubernetes rejected those pod hostnames. Corrected database names leave room for
the suffix; names for registries of 52 characters or fewer are unchanged.

If a Secret, StatefulSet or retained PVC still uses the earlier name, the sync
webhook returns a `legacy managed database` error instead of returning replacement
children. The error identifies the resource and corrected name. Existing children
are left in place; the controller does not rename PVCs or generate replacement
credentials over this state. Inspect Metacontroller's webhook error when this
guard triggers; the AgentRegistry's previous status may remain unchanged.

Preserve the original database Secret and inspect or back up any retained volume
before removing obsolete resources. If the failed installation is disposable and
its storage is confirmed empty, remove its AgentRegistry, wait for owned children
to disappear, explicitly remove its retained PVC, then recreate the registry.
For data that must be kept, restore a verified backup into a separate installation
and validate it before retiring the old resources. Restoring the old PVC under its
old name does not attach it to the newly named StatefulSet.

## Backups and availability

PostgreSQL owns catalog persistence. Back up each tenant database and protect the
authentication Secret, including the JWT key. For managed PostgreSQL, also back
up its immutable generated Secret (`catalog-postgres` in the examples). A retained
volume is not a backup, and its stored passwords require that original Secret.
Test restoration before relying on backups. The operator has no backup or
restore API and does not provide database replication or automatic failover.

Each registry runs one application pod, and changes use `Recreate`. It is not a highly available
application deployment. A multi-node cluster improves scheduling options but does
not remove planned rollout interruptions. Resource settings apply only to the
upstream application; account for the gateway and managed PostgreSQL's additional
fixed resources.

## Delete a registry

Before deletion, back up any database data and generated credentials you intend
to recover.

```sh
kubectl --context "$KUBE_CONTEXT" -n tenant-a delete agentregistry catalog
kubectl --context "$KUBE_CONTEXT" -n tenant-a \
  get deployment/catalog service/catalog serviceaccount/catalog networkpolicy/catalog
```

Wait until the application children and,
in managed mode, the PostgreSQL StatefulSet, Service, ConfigMap, NetworkPolicy
and generated Secret are gone. The `data-catalog-postgres-0` PVC is retained.
Supplied auth/TLS Secrets, external databases, namespaces and unrelated resources
remain. Retain or remove these separately according to your data-retention policy.

Recreate the original AgentRegistry first. It finds the retained PVC and stops
until the original `catalog-postgres` Secret is restored. Restore that Secret from
backup using its original `data`, `type` and `immutable` fields, with only its
name and namespace in metadata. Remove obsolete UID, resourceVersion, owner
references and labels. Then label it with the **new** parent's UID so that
Metacontroller can adopt it:

```sh
REGISTRY_UID="$(kubectl --context "$KUBE_CONTEXT" -n tenant-a get agentregistry catalog -o jsonpath='{.metadata.uid}')"
kubectl --context "$KUBE_CONTEXT" -n tenant-a label secret catalog-postgres \
  controller-uid="$REGISTRY_UID" --overwrite
kubectl --context "$KUBE_CONTEXT" -n tenant-a \
  wait --for=condition=Ready agentregistry/catalog --timeout=5m
```

A restored Secret without this label cannot be adopted by the bundled controller.
Restore database contents from backup if the volume is not available. Delete a
retained PVC only when its data is no longer needed and you intend to initialize
a fresh database.

Before uninstalling the operator, inspect all namespaces and finish any intended
registry deletions while the controller is running:

```sh
kubectl --context "$KUBE_CONTEXT" get agentregistries --all-namespaces
helm uninstall agentregistry-operator \
  --kube-context "$KUBE_CONTEXT" --namespace agentregistry-system
```

Removing the operator while registries remain stops reconciliation and repair.
Helm leaves CRDs installed. Deleting the AgentRegistry CRD deletes all registry
objects in the cluster, so keep CRD deletion as a separate deliberate operation.

## Troubleshooting

| Symptom | Inspect | Usual correction |
| --- | --- | --- |
| `DependencyNotReady` | `kubectl describe areg catalog` | Create the named Secret; correct required keys, URI host, TLS mode or certificate |
| No pod / Deployment scaled to zero | `DependenciesReady` and condition message | Repair dependencies; the operator intentionally stops invalid deployments |
| `DependenciesReady` stays at `Provisioning`, no PostgreSQL StatefulSet | Metacontroller logs and `<registry>-postgres` Secret | Fix Secret-creation failures such as quota, admission or RBAC errors; provisioning retries automatically after credentials are persisted |
| PostgreSQL pod Pending / PVC Pending | PVC events and StorageClass | Supply a working default or selected StorageClass and enough capacity |
| Managed credentials missing or invalid | Condition message and `<registry>-postgres` Secret | Restore original credentials from backup; never invent a new password for existing storage |
| `ImagePullBackOff` | Pod events | Publish/load the image for the node architecture and ensure registry access |
| `Progressing`, application restarting | Registry container logs | Check database credentials, role permissions, extension installation and migrations |
| `Progressing`, readiness failing | Gateway logs, application logs and database connectivity | Check database query/MCP readiness and certificate validity |
| Gateway exits with `gateway listener failed` | Gateway logs and container exit status | Fix the reported listening address or port conflict; either listener failing stops both servers and exits nonzero |
| Registry cannot reach an external local database | Database pod labels, namespace baseline and database ingress policy | Label DB pods `registry.deepai.cloud/database: catalog`, permit TCP 5432 ingress |
| Remote database TLS failure | URI, `ca.crt`, server certificate | Use `verify-full`, `/database/ca.crt` and an IP SAN matching `database.host` |
| Service external address stays pending | Service events and load-balancer controller | Provide LoadBalancer capacity; check port conflicts on K3s |
| Public URL fails while Ready | DNS, external routing and client trust | Configure DNS/LB and full certificate chain; readiness cannot test these |
| HTTP 401 | Client authentication | Send configured Basic-auth username/password |
| HTTP 403 for an execution operation | Requested API/MCP method | Execution operations are deliberately outside catalog scope |
| Spec changed but status does not advance | Metacontroller logs | Correct validation errors and verify webhook/RBAC connectivity |

Get the two application-container logs independently:

```sh
kubectl --context "$KUBE_CONTEXT" -n tenant-a logs deployment/catalog -c registry --tail=100
kubectl --context "$KUBE_CONTEXT" -n tenant-a logs deployment/catalog -c gateway --tail=100
# Managed database only:
kubectl --context "$KUBE_CONTEXT" -n tenant-a logs statefulset/catalog-postgres --tail=100
kubectl --context "$KUBE_CONTEXT" -n agentregistry-system \
  logs deployment/agentregistry-operator --tail=100
kubectl --context "$KUBE_CONTEXT" -n agentregistry-system get pods
```

Use the actual Metacontroller pod name from the last command to read its logs.
Avoid sharing Secret manifests or raw credential files in issue reports. Include
the AgentRegistry spec/status with sensitive infrastructure details removed,
container states, events, chart/image versions and relevant redacted log excerpts.
