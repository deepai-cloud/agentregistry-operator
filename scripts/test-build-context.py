#!/usr/bin/env python3
"""Check Docker's source exclusions with synthetic files and an offline scratch build."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]


def main():
    with tempfile.TemporaryDirectory(prefix="areg-build-context-") as temporary:
        work = Path(temporary)
        context = work / "context"
        context.mkdir()
        shutil.copy2(ROOT / ".dockerignore", context / ".dockerignore")
        # Exercise the real allowlist without sending any checkout contents or
        # credentials to the daemon. No base image, container, or tag is needed.
        expected = {"LICENSE", "go.mod", "cmd/operator/main.go", "cmd/gateway/main.go",
                    "internal/gateway/gateway.go", "internal/operator/sync.go"}
        excluded = {"README.md", "docs/private.txt", ".local/credentials.txt", ".cache/token",
                    ".git/config", "cmd/operator/main_test.go", "internal/gateway/gateway_test.go"}
        for prefix in ("cmd", "cmd/operator", "internal", "internal/gateway"):
            excluded.update(prefix + "/" + name for name in (
                ".local/credentials.txt", ".cache/token", ".git/config", ".DS_Store",
                "__pycache__/helper.pyc", "helper.pyc", "testdata/fixture.json",
                "testdata/probe/main.go",
            ))
        for name in expected | excluded:
            path = context / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("synthetic build context fixture: " + name + "\n")
        probe = work / "Dockerfile"
        probe.write_text("FROM scratch\nCOPY . /\n")
        output = work / "output"
        result = subprocess.run([
            "docker", "build", "--network=none", "--file", str(probe),
            "--output", "type=local,dest=" + str(output), str(context),
        ], env={**os.environ, "DOCKER_BUILDKIT": "1"}, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
        if result.returncode:
            raise SystemExit("Build context probe failed:\n" + result.stdout + result.stderr)
        actual = {path.relative_to(output).as_posix() for path in output.rglob("*") if path.is_file()}
        if actual != expected:
            raise SystemExit("Incorrect Docker build context; unexpected files: "
                             + repr(sorted(actual - expected)) + "; missing source files: "
                             + repr(sorted(expected - actual)))
        print("PASS: Docker retains production sources and excludes nested local state, caches and test fixtures")


if __name__ == "__main__":
    main()
