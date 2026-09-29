# Managed PostgreSQL acceptance

Run `make test-e2e` (or the equivalent `make test-k3s`) with Docker running.
Install Go 1.25+, Helm, kubectl, Python 3, and **k3d 5.9.0**. The runner finds
`k3d` on `PATH` or in `.cache/tools/k3d`; `K3D` can specify another path. Set
`DOCKER_CONTEXT` when needed. If the current Docker daemon is unavailable and no
context override was supplied, the runner tries the `desktop-linux` context
without changing Docker's saved default.

The runner creates its own randomly named K3s cluster using
`rancher/k3s:v1.34.11-k3s1@sha256:5d52389a0f4fd7ebdb5a1fb2d7c67c35da966230782c4abb0667d86bcccea9c2`.
It uses a private kubeconfig and explicit Kubernetes context for every operation;
the existing kubeconfig, current context, and retained clusters are untouched.
The cluster is deleted on completion or failure. A forced process termination
may leave its uniquely named `areg-managed-*` cluster for manual cleanup.

The test builds the current operator source, installs the local chart once,
and creates an `AgentRegistry` with only `version` and `authSecretName`.
It verifies:

- The same Helm release installs the operator and vendored Metacontroller.
- PostgreSQL's Secret, StatefulSet, and persistent storage are provisioned.
- Live admission rejects database mode/storage changes and invalid external
  database combinations.
- `pg_trgm` and `vector` exist; the application login cannot administer roles,
  create databases, replicate, or bypass row security.
- Registry readiness, anonymous HTTP rejection, and authenticated catalog reads.
- Stable credentials and saved database content through a PostgreSQL pod restart.
- Registry deletion removes owned children and retains the PVC and supplied auth
  Secret.
- Recreating the registry over retained storage blocks credential regeneration;
  restoring the original database Secret with the recreated parent's
  `controller-uid` selector label recovers readiness and saved data.

Network access is required for pinned container images on first use. Credentials
are generated per test, sent on stdin, never printed, and destroyed with the
cluster. Non-secret image/version/check evidence is kept under
`.cache/areg-managed-*/result.json`. `make verify` runs unit tests, static checks,
builds, offline chart verification, and Python syntax checks without a cluster.
