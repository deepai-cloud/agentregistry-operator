# Packaging and releases

The [deepai-cloud/agentregistry-operator repository](https://github.com/deepai-cloud/agentregistry-operator)
distributes one operator/gateway image, one Helm chart and downloadable source
and executable archives through GitHub Actions. The chart
vendors Metacontroller 4.17.2, including its CRDs, so installation needs a single
Helm release. Each registry gets managed PostgreSQL by default, or can use an
external database. Registry authentication Secrets are supplied separately.

## Build a package locally

From the repository root, with Go 1.26.10, Python 3 and Helm 3 available:

```sh
make verify
make package VERSION=0.1.0 IMAGE=registry.example.com/platform/agentregistry-operator:v0.1.0
```

The Docker builder and all CI jobs use Go 1.26.10, a supported release containing
[standard-library security fixes](https://go.dev/doc/devel/release#go1.26.10).
The `go.mod` toolchain directive selects the same version for local builds with
Go's default automatic toolchain selection; the first run may download it.

Replace the image with your own built and pushed image. Packaging does not build
or upload it. `IMAGE` accepts an explicit tag or `repository@sha256:...` digest;
the packaged chart uses that reference for both the webhook and tenant gateways.
Without overrides, `make package` uses the chart version and the local `dev` image.

The output is:

| Artifact | Contents |
| --- | --- |
| `dist/agentregistry-operator-0.1.0.tgz` | Installable chart with the selected image and bundled dependency |
| `dist/agentregistry-operator-0.1.0-bundle.tar.gz` | Chart archive, source, docs, runnable examples and `RELEASE.json` |
| `dist/agentregistry-operator-0.1.0-linux-amd64.tar.gz` | Linux amd64 operator and gateway executables, license and release metadata |
| `dist/agentregistry-operator-0.1.0-linux-arm64.tar.gz` | Linux arm64 operator and gateway executables, license and release metadata |
| `dist/SHA256SUMS` | SHA-256 checksums for all archives from this packaging run |

Only explicit public source directories enter the source bundle. Top-level local
state, caches, binaries and build outputs are excluded from that bundle.
Packaging rejects symlinks anywhere along a selected source path, including
a symlinked `.github` parent of `.github/workflows`.
Keep credentials, kubeconfigs and other private files under `.local/`; local state placed
under the chart's `.local/`, `.cache/` or `__pycache__/` directories is excluded
from both the installable chart and source bundle, including nested directories.
These exclusions also apply when packaging the chart directly with Helm. Arbitrary
private files elsewhere in public source directories are not automatically detected.
The executable archives are built separately from the release source. Store each release
in its own destination; `SHA256SUMS` describes the latest packaging run.

When rebuilding a release, the packager replaces each output file under `dist/`
after all copies finish, with `SHA256SUMS` last. If an output filename is a
symlink or hard link, other files it points to keep their contents. An existing
directory at an output filename stops the run before any release files are replaced.

`make verify` packages temporary fixture checkouts and checks image references,
release metadata, checksums, private-state exclusions and symlink rejection without
modifying existing release files. Binary packaging tests use a compiler stand-in;
the CI package build compiles the real Linux executables for both architectures.

Remote-database tests in `make verify` check IPv4 and IPv6 configuration, including
private and IPv4-mapped addresses. They require verified TLS and database egress
restricted to the configured IP on TCP 5432, and reject loopback, link-local,
unspecified and multicast addresses. These tests use generated resources and
synthetic CA certificates without connecting to a database or cluster.

Database example checks parse the actual YAML with Helm and pass the manifests
and locally generated test Secrets through the operator's reconciliation code.
They verify that the external example uses the supplied database, creates only
application resources, and selects the example PostgreSQL pods for network access.
The managed examples must provision their documented volume sizes after credentials
are persisted. These checks also run in `make verify`, without Docker or a cluster.

The source bundle and Helm chart include the project's MIT `LICENSE`. The operator
image includes it at `/LICENSE` and declares the `MIT` OCI license identifier.
Vendored Metacontroller retains its separate Apache 2.0 license notice.

Install the chart archive after making its selected image accessible to nodes:

```sh
helm upgrade --install agentregistry-operator dist/agentregistry-operator-0.1.0.tgz \
  --kube-context "$KUBE_CONTEXT" \
  --namespace agentregistry-system --create-namespace --wait --timeout 5m
```

After extracting a bundle, its chart archive is at the bundle root. The included
source chart retains the repository defaults; use the versioned `.tgz` to get the
selected release image. Follow [creating a registry](creating-a-registry.md) next.

## Automatic publishing

The [release workflow](../.github/workflows/release.yaml) runs automatically on
pushes to `main` in the canonical repository and on version tags such as `v0.1.0`
or `v0.2.0-rc.1`. It can also be started manually from GitHub Actions on `main`.
Feature branch pushes and pull requests run verification without publishing.

Each publishing run verifies the source, packages the chart and Linux amd64/arm64
executables, passes the managed PostgreSQL acceptance test, builds the
operator/gateway image for both architectures, publishes
the image and OCI chart to GHCR, then attaches the archives and checksums to a
GitHub release. No local build is required.

| Trigger | Published version | GitHub release |
| --- | --- | --- |
| Push to `main` or manual run on `main` | `<chart-base-version>-dev.<run-id>.<attempt>.g<12-character-sha>` | Prerelease tagged `dev-<run-id>-<attempt>-<12-character-sha>` |
| Push `v0.1.0` | `0.1.0` | Release tagged `v0.1.0` |
| Push `v0.2.0-rc.1` | `0.2.0-rc.1` | Prerelease tagged `v0.2.0-rc.1` |

For main builds, the base version comes from `Chart.yaml` with any prerelease
suffix removed. The run ID, attempt and commit suffix give every run a distinct
version. Use the exact chart version and image reference from the release notes;
the main build's GitHub release tag is different from its package version.
Images use `v<version>` tags. There are no moving `main` or `latest` image tags.
Development prereleases do not replace the latest stable GitHub release.

Find both tagged releases and main build prereleases on the
[releases page](https://github.com/deepai-cloud/agentregistry-operator/releases).
Every release includes the chart `.tgz`, source/examples bundle, Linux amd64 and
arm64 executable archives, and `SHA256SUMS`. Each executable archive contains both
`operator` and `gateway`, the MIT license and release metadata. Container images
are pulled from GHCR; offline container image archives are not included.

The operator/chart version is separate from `AgentRegistry.spec.version`, which
selects the upstream registry application version.

## Publishing setup

For `deepai-cloud/agentregistry-operator`, version `0.1.0` has these coordinates:

- Image: `ghcr.io/deepai-cloud/agentregistry-operator:v0.1.0`
- Helm chart: `oci://ghcr.io/deepai-cloud/charts/agentregistry-operator`, version `0.1.0`
- Downloadable chart, source bundle, executable archives and checksums: [GitHub releases](https://github.com/deepai-cloud/agentregistry-operator/releases)

The chart's source metadata and image's OCI labels identify the source repository.

Enable GitHub Actions and allow the workflow's `GITHUB_TOKEN` to write packages
and releases. No additional registry password is required. If either GHCR package
already exists, grant this repository access under the package's **Manage Actions
access** settings. Newly created GHCR packages are private by default; after the
first publishing run, make **both** the image and chart packages public for installation
without registry credentials. See [GitHub's container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry).

Once this setup is complete, pushing to `main` publishes a development prerelease
automatically. A maintainer can also use **Run workflow** with the `main` branch
in GitHub Actions to publish the current revision.

## Publish a version

To publish a stable version or a named prerelease from a reviewed, committed
revision, push its version tag:

```sh
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

Choose an unused version. Tags must use a semantic version without build metadata;
an optional prerelease suffix such as `-rc.1` is supported. The tag sets the chart
version, chart application version and operator/gateway image tag. The workflow
verifies the code and packaged chart before publishing, and includes the image
manifest digest and package coordinates in its generated release notes.

Kubernetes display labels are shortened to 63 characters and trimmed to valid
label boundaries when necessary. The full operator version remains in the
`registry.deepai.cloud/operator-version` annotation on the parent chart's
templated resources; chart versions and image tags retain their exact values.

After the release workflow succeeds and package access is configured, verify image
and chart access from a client without saved GHCR credentials:

```sh
docker pull ghcr.io/deepai-cloud/agentregistry-operator:v0.1.0
helm show chart oci://ghcr.io/deepai-cloud/charts/agentregistry-operator --version 0.1.0
```

The registry pods use the same published image as the operator. Install a published
release using the selected Kubernetes context:

```sh
helm upgrade --install agentregistry-operator \
  oci://ghcr.io/deepai-cloud/charts/agentregistry-operator --version 0.1.0 \
  --kube-context "$KUBE_CONTEXT" \
  --namespace agentregistry-system --create-namespace --wait --timeout 5m
```

Source and release builds use the same Dockerfile and packaging command. CI
performs all publishing for main builds and version tags; none of the local build,
package or demo commands publish artifacts.

## Dependencies and verification

The vendored Metacontroller archive is verified against its SHA-256 checksum by
`make chart`. If intentionally updating the dependency, update the chart version,
pin and checksum in `scripts/fetch-metacontroller.py` together, inspect upstream
CRD/RBAC changes and repeat chart and live installation checks. The existing
helper downloads the pinned dependency only when it is missing; `--verify`
performs offline verification.

CI runs Go race tests, builds, Helm contract checks and documentation link checks,
and builds both container architectures. Both verification and release workflows
call the shared [managed PostgreSQL acceptance workflow](../.github/workflows/managed-postgres.yaml)
after source verification succeeds. Publishing requires both jobs to pass.

`make test-build-context` checks the actual Docker build context with synthetic
source files and nested local state. It uses an offline `FROM scratch` build and
temporary local output; it does not create a tagged image or run a container.
The build retains production sources but excludes `.local`, `.cache`, `.git`,
Python caches and Go test fixtures, including inside `cmd/` and `internal/`.
Both workflows run this check in the shared acceptance job. Run it locally with a
working Docker daemon, selecting `DOCKER_CONTEXT` when needed. Keep private files
in `.local/`; arbitrary private files elsewhere in source directories are not
automatically detected.

`make verify` also starts a local gateway subprocess and rotates a simulated
Kubernetes TLS Secret mount. Trusted HTTPS connections must use the replacement
certificate, reject mismatched keys and malformed certificates, and recover after
the mount is repaired without restarting the process. Invalid material must also
fail readiness. This test requires neither a registry backend nor a cluster.

Gateway proxy tests also exercise local HTTP and HTTPS listeners. They verify
that client credentials, cookies, origin and forwarding headers stay at the
gateway while API request contents and MCP session/protocol headers reach the
backend. Cross-origin and cross-site browser requests must be rejected before
either backend is called; same-origin browsers and non-browser clients remain
supported. These checks run in `make verify` without a cluster.

The acceptance job runs `make test-registry-probes` to exercise the generated
health command inside both pinned registry images. A temporary loopback server
provides healthy, failed and stalled responses, followed by recovery. This checks
the images' actual curl executable, timeout, and resistance to curl configuration
and proxy environment settings. Containers run as non-root with a read-only root
filesystem, no networking and no published ports, and are removed by ownership
label and immutable ID. Run this target with a local Docker daemon and Go; images
are pulled only when absent. This is a probe compatibility test, not a test of the
upstream registry server or its database.

The job next runs `make test-postgres-bootstrap` with the pinned
PostgreSQL image in disposable Docker containers. It interrupts setup after role,
database and extension creation, checks recovery after a SQL failure, and verifies
that existing data and credentials survive retries and completed-volume restarts.
These tests use no cluster or published ports and remove their anonymous volumes.
Cleanup finds containers by a unique label for each creation attempt and removes
them by ID, including when `docker create` times out or is interrupted after the
daemon creates the container. A reused container name or another run's label
cannot select unrelated containers for deletion. The Docker daemon must remain
reachable for cleanup. `make verify` exercises these failure paths without Docker.

It then runs `make test-e2e` on a disposable K3s cluster using pinned
k3d 5.9.0 and kubectl 1.34.11 binaries verified by SHA-256. It tests installation,
live admission, database permissions and extensions, authenticated HTTP, restart
persistence, deletion with PVC retention, and recovery with original credentials.
Admission checks include the shared HTTPS hostname cases for both creation and
updates, covering literal IPs and DNS length boundaries. These requests use
server-side dry runs; valid hostnames do not change the private test registry or
provision a LoadBalancer. Connection or permission failures fail the check rather
than counting as an expected validation rejection.
It uses a private kubeconfig and removes its own cluster on completion. The
`managed-postgres-evidence` artifact retains only non-secret `result.json` reports
for 14 days, including failed-run reports when available; kubeconfigs are excluded.
Cluster deletion is attempted even if stopping the local port-forward or closing
its log fails. Cleanup errors fail the run and are reported together. If cluster
deletion fails, the private kubeconfig remains available for manual recovery.
`make verify` tests these cleanup failure paths without Docker or Kubernetes.

Acceptance command failures and diagnostic output redact the run's known
passwords, JWT key and database URL in both plaintext and base64-encoded Secret
form. This covers credentials echoed in kubectl patch errors while preserving
the surrounding error message. `make verify` checks these log paths with
synthetic credentials; successful Secret reads remain intact for recovery tests.

The acceptance runner lets kubectl bind an available port on `127.0.0.1` and
waits for its listener announcement before issuing catalog requests. This avoids
port collisions when acceptance jobs run concurrently on one host.
Its local HTTP checks ignore proxy environment settings and
reject redirects. Generated Basic credentials are sent only to the intended
loopback port-forward, and another service cannot satisfy the registry check by
returning a successful response after a redirect. After restarting PostgreSQL,
the runner retries dropped connections, timeouts and incomplete responses within
the existing recovery timeout. An exited port-forward is reported as a failure
instead of retried. `make verify` tests these boundaries and recovery behavior
with local mock servers, without Docker or Kubernetes.

Run `make test-e2e` locally with Docker, k3d 5.9.0 and kubectl installed to exercise
the same acceptance test. `make demo-up` creates the persistent local example.
Both are separate from offline verification.
Helm CRDs require explicit schema upgrades; follow the
[operations guide](operations.md#upgrade-the-operator-and-chart).
