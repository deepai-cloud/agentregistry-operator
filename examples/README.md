# Registry examples

Start with the [local quickstart](../docs/quickstart.md) for a complete working
installation. The operator creates persistent PostgreSQL and its credentials by
default; you only supply an authentication Secret.

| Example | Purpose | Before applying |
| --- | --- | --- |
| [namespace-baseline.yaml](namespace-baseline.yaml) | `tenant-a` and default-deny ingress | A CNI that enforces NetworkPolicy |
| [private.yaml](private.yaml) | Minimal `catalog` registry with managed PostgreSQL | Auth Secret and a default StorageClass |
| [managed-registry.yaml](managed-registry.yaml) | Minimal managed-database example | Same as `private.yaml` |
| [tls.yaml](tls.yaml) | HTTPS LoadBalancer, managed database, explicit app resource sizing | Auth Secret, trusted certificate, storage, load balancer and DNS |
| [managed-storage.yaml](managed-storage.yaml) | Managed PostgreSQL with 10Gi storage | Auth Secret and a default StorageClass; choose storage before creation |
| [external-database.yaml](external-database.yaml) | Private registry using your own namespace-local PostgreSQL | Database and auth Secrets; database network access |
| [remote-database.yaml](remote-database.yaml) | HTTPS registry using a remote database IP | Replace IP/hostname; database certificate with IP SAN and CA Secret |
| [local/postgres.yaml](local/postgres.yaml) | Optional disposable external PostgreSQL | External-mode generated Secrets; not needed for the quickstart |
| [local/create-secrets.py](local/create-secrets.py) | Generate random local auth credentials | Python 3; output under ignored `.local/quickstart/` |

Choose one registry manifest before creating the resource. These examples use the
same `tenant-a/catalog` identity. Database mode, size and StorageClass are immutable
after creation. In particular, preserve any custom database configuration on your
existing registry when enabling HTTPS. Both `private.yaml` and `tls.yaml` omit
database settings and use the managed default of 1Gi.

For separate registries, choose unique names, namespaces and credentials. The
operator gives each managed registry its own PostgreSQL instance, role and volume.
Deleting a managed registry retains its PVC but deletes the generated database
Secret. Back up both the database contents and its Secret for recovery.

The optional `local/postgres.yaml` is for exercising external database mode. It
uses `emptyDir`, so replacing its pod loses data. To generate its three required
Secrets, run:

```sh
python3 examples/local/create-secrets.py --external-database \
  --output-dir .local/external-example
```

Apply the generated `secrets.json` before `local/postgres.yaml` and
`external-database.yaml`.

The generated Secret manifest and credential files contain plaintext credentials;
keep them out of version control. See [creating a registry](../docs/creating-a-registry.md)
for managed storage, TLS and external database configuration.
