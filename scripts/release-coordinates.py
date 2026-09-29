#!/usr/bin/env python3
"""Derive safe, matching image/chart/release names for GitHub Actions."""
import os
from pathlib import Path
import re
import runpy

ROOT = Path(__file__).resolve().parents[1]
VERSION_PATTERN = runpy.run_path(str(ROOT / "scripts/package.py"))["VERSION_PATTERN"]


def coordinates(env, chart_version):
    repository = env.get("GITHUB_REPOSITORY", "").lower()
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9_.-]*", repository):
        raise ValueError("GITHUB_REPOSITORY must identify an owner/repository")
    sha = env.get("GITHUB_SHA", "")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("GITHUB_SHA must identify the full source commit")
    event = env.get("GITHUB_EVENT_NAME")
    ref = env.get("GITHUB_REF", "")
    snapshot = ref == "refs/heads/main" and event in {"push", "workflow_dispatch"}
    if snapshot:
        if not VERSION_PATTERN.fullmatch(chart_version):
            raise ValueError("Chart version must be a semantic version without build metadata")
        run_id = env.get("GITHUB_RUN_ID", "")
        attempt = env.get("GITHUB_RUN_ATTEMPT", "")
        if not all(re.fullmatch(r"[1-9][0-9]*", value) for value in (run_id, attempt)):
            raise ValueError("Development releases need a positive run ID and attempt")
        base = chart_version.split("-", 1)[0]
        version = f"{base}-dev.{run_id}.{attempt}.g{sha[:12]}"
        release_tag = f"dev-{run_id}-{attempt}-{sha[:12]}"
    elif event == "push" and ref.startswith("refs/tags/v"):
        version = ref.removeprefix("refs/tags/v")
        if not VERSION_PATTERN.fullmatch(version):
            raise ValueError("Release tags must be vMAJOR.MINOR.PATCH with an optional SemVer prerelease")
        release_tag = f"v{version}"
    else:
        raise ValueError("Publishing requires a main push, a version tag push, or a manual run on main")
    if len(f"v{version}") > 128:
        raise ValueError("Release version exceeds the container image tag limit")
    return {
        "version": version,
        "image": f"ghcr.io/{repository}:v{version}",
        "chart_registry": f"oci://ghcr.io/{repository.split('/')[0]}/charts",
        "release_tag": release_tag,
        "prerelease": str("-" in version).lower(),
        "snapshot": str(snapshot).lower(),
    }


def main():
    chart = (ROOT / "charts/agentregistry-operator/Chart.yaml").read_text()
    chart_version = re.search(r"(?m)^version: (.+)$", chart)[1]
    try:
        result = coordinates(os.environ, chart_version)
    except ValueError as error:
        raise SystemExit(str(error)) from error
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        for name, value in result.items():
            output.write(f"{name}={value}\n")
    print(f"Publishing {result['version']} from {os.environ['GITHUB_SHA']}")


if __name__ == "__main__":
    main()
