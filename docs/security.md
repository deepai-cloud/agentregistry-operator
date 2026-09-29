# Security and tenancy

The operator installs a catalog service. It does not execute registered agents,
run MCP tools against external systems, deploy workloads, or mount a Docker socket.
This separation is enforced by both the gateway and the generated pod/network
configuration.

## Authentication and transport

Every UI, API and MCP client request requires the registry's Basic-auth credentials.
The gateway removes client credentials and forwarding headers before proxying to
loopback-only application backends. It restricts API/MCP routes to catalog
operations and rejects cross-origin browser requests. Upstream UI execution
controls may still be visible; those operations are blocked.

A private registry uses HTTP within its namespace. Basic authentication does not
encrypt traffic. Enable TLS exposure when the network path requires encryption.
Public TLS mode exposes a LoadBalancer on 443; the platform supplies certificates,
renewal, DNS and external reachability.

There is one configured username/password per registry, not a per-user identity or
role system. Share a registry only within a trust group that can share those
credentials and its catalog access.

## Runtime and network boundaries

Registry pods run as non-root, use read-only root filesystems, drop capabilities,
disable privilege escalation, and have no automounted Kubernetes token. They have
no Kubernetes RBAC. The application can write only its bounded temporary volume;
its catalog data lives in PostgreSQL. Managed PostgreSQL also runs non-root
without a Kubernetes token and uses its own persistent data volume.

The generated NetworkPolicy allows database TCP 5432 and cluster DNS egress.
Managed PostgreSQL receives its own policy, allowing TCP 5432 ingress only from
its registry's pods, and no outgoing connections. An external local database must
have the registry-specific database pod label in the same namespace. A remote
database is allowed only at its configured literal IP. Private
registry ingress is namespace-local; exposed HTTPS ingress permits external
clients through the Service.

These guarantees require a NetworkPolicy-enforcing CNI and the tenant namespace
baseline. Policies are additive: another allow policy can broaden traffic. The
operator's policy does not replace PostgreSQL authorization, platform firewalls,
Pod Security policy or namespace governance. Managed PostgreSQL connections are
plaintext within this network boundary. Choose an external database with
`verify-full` when encrypted database transport is required.

## Administrative trust

The operator and Metacontroller share the Helm release namespace. Their webhook
connection is namespace-local: its NetworkPolicy permits TCP 8080 only from
the bundled Metacontroller pods and denies all webhook egress. The webhook has
no Kubernetes API token or RBAC grants.

Metacontroller is still a trusted cluster administration component. Its shared
informers read Secrets and namespaces cluster-wide. Its
explicit RBAC also manages the declared child resource kinds. Namespaced related
Secret selection limits what is delivered to a registry reconciliation; it does
not eliminate Metacontroller's cluster-wide cache access. Upstream 4.17.2's
[informer factory](https://github.com/metacontroller/metacontroller/blob/v4.17.2/pkg/dynamic/informer/factory.go)
does not support a namespace-scoped cache. The CRDs and CompositeController
are also cluster-scoped; a namespace-only installation is not supported by this
architecture. Do not replace the explicit ClusterRole with a Role: its watches
would fail. Tenant registry workloads and their references remain namespace-local.

Restrict changes to the webhook, Metacontroller, CompositeControllers and their
RBAC to platform administrators. Tenant users must not be able to change managed
children, referenced Secrets, NetworkPolicies or namespace labels beyond your
intended security policy. Kubernetes namespace/cluster administrators are outside
this tenant-isolation boundary.

Managed mode creates an independent database and random credentials for each
registry. The runtime database role owns that database but cannot create roles or
databases and is not a superuser. The application receives only its connection
URL; the PostgreSQL administrator password is not mounted in its pod. Managed
database Secrets are immutable and must be backed up with the database. The
operator does not support rotating them in place.

For external PostgreSQL, supply independent databases and credentials per tenant.
Protect Secret sources and backups, and coordinate password rotation with the
database before updating the URI Secret. The examples generate random
authentication credentials locally and never embed fixed passwords in tracked
manifests.

## Data lifecycle

Deleting an AgentRegistry removes its four application children and, in managed
mode, its PostgreSQL StatefulSet, Service, ConfigMap, NetworkPolicy and generated
Secret. The database PVC is retained, including when the StatefulSet scales down.
Recovering that data requires the original generated Secret as well as the volume
or a database backup. The operator refuses to generate replacement credentials
when it detects existing database storage without its Secret. External databases,
supplied authentication/TLS Secrets, namespaces and backups remain independently
managed. The local demo uses a PVC, but deleting its cluster destroys that storage.
