# Packaging and releases

The [deepai-cloud/agentregistry-operator repository](https://github.com/deepai-cloud/agentregistry-operator)
distributes one operator/gateway image, one Helm chart and downloadable source
and executable archives through GitHub Actions. The chart
vendors Metacontroller 4.17.2, including its CRDs, so installation needs a single
Helm release. Each registry gets managed PostgreSQL by default, or can use an
external database. Registry authentication Secrets are supplied separately.

## Build a package locally

From the repository root, with Go 1.25.4, Python 3 and Helm 3 available:

```sh
make verify
make package VERSION=0.1.0 IMAGE=registry.example.com/platform/agentregistry-operator:v0.1.0
```

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

Only explicit public source directories enter the source bundle. Local credentials,
kubeconfigs, caches, binaries and build outputs are excluded from that bundle.
The executable archives are built separately from the release source. Store each release
in its own destination; `SHA256SUMS` describes the latest packaging run.

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
executables, builds the operator/gateway image for both architectures, publishes
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
and builds both container architectures. `make demo-up` exercises the full local
installation and registry readiness path. It is separate from offline verification.
Helm CRDs require explicit schema upgrades; follow the
[operations guide](operations.md#upgrade-the-operator-and-chart).
