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

## Rotate credentials and certificates

Update authentication and TLS Secrets through your secret manager. Keep all
required keys and the same Secret name. The controller notices Secret changes
and rolls out the registry. Rollouts use `Recreate`, causing a service interruption.

Managed database Secrets are immutable; in-place database password rotation is
not supported. Back up their original contents, and restore the original Secret
if it is lost. Do not delete it to trigger credential regeneration: the operator
refuses to generate new passwords when it detects the existing StatefulSet or PVC.

For an external database, arrange for new credentials to be valid before updating
the URI Secret. Changing a Kubernetes Secret alone does not change PostgreSQL
passwords. Keep issuer chains and hostnames correct when
rotating the TLS Secret. Deleting a required Secret, or replacing it with invalid
contents, scales the registry to zero until it is repaired.

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
| PostgreSQL pod Pending / PVC Pending | PVC events and StorageClass | Supply a working default or selected StorageClass and enough capacity |
| Managed credentials missing or invalid | Condition message and `<registry>-postgres` Secret | Restore original credentials from backup; never invent a new password for existing storage |
| `ImagePullBackOff` | Pod events | Publish/load the image for the node architecture and ensure registry access |
| `Progressing`, application restarting | Registry container logs | Check database credentials, role permissions, extension installation and migrations |
| `Progressing`, readiness failing | Gateway logs, application logs and database connectivity | Check database query/MCP readiness and certificate validity |
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
