#!/usr/bin/env python3
"""Run generated health probes inside both pinned registry images.

Requires Docker, Go and Python 3. Uses disposable containers without networking
or published ports; only a temporary test directory is mounted, read-only.
The actual registry server is replaced by a controlled loopback health endpoint.
"""

import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time


ROOT = Path(__file__).resolve().parents[1]
HELPER = "./internal/operator/testdata/registry-probe"
OWNER_LABEL = "registry.deepai.cloud/probe-test-owner"


def run(*args, timeout=60, check=True, **kwargs):
    result = subprocess.run(args, cwd=ROOT, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout, **kwargs)
    if check and result.returncode:
        raise RuntimeError(f"{args[0]} {args[1]} failed: {result.stderr.strip()}")
    return result


def check_probe(runtime, check):
    token = secrets.token_hex(32)
    name = "areg-probe-" + token[:12]
    image = check["image"]
    if run("docker", "image", "inspect", image, check=False).returncode:
        run("docker", "pull", image, timeout=180)
    (runtime / "state").write_text("healthy")
    try:
        # Label before creation, then discover by that label even if the create
        # client times out after the daemon has accepted the container.
        run("docker", "create", "--name", name, "--label", OWNER_LABEL + "=" + token,
            "--network", "none", "--read-only", "--user", "1001:1001", "--cap-drop", "ALL",
            "--security-opt", "no-new-privileges", "--mount", f"type=bind,source={runtime},target=/probe,readonly",
            "--env", "http_proxy=http://127.0.0.1:1", "--env", "all_proxy=http://127.0.0.1:1",
            "--env", "no_proxy=", "--env", "NO_PROXY=", "--env", "CURL_HOME=/probe",
            "--entrypoint", "/probe/server", image)
        run("docker", "start", name)
        command = ("docker", "exec", name, *check["command"])
        deadline = time.monotonic() + 20
        while run(*command, timeout=5, check=False).returncode:
            if time.monotonic() >= deadline:
                raise RuntimeError("Healthy probe failed for registry " + check["version"])
            time.sleep(0.2)
        for state, expected in (("failed", 22), ("stalled", 28), ("healthy", 0)):
            (runtime / "state").write_text(state)
            started = time.monotonic()
            probe = run(*command, timeout=5, check=False)
            elapsed = time.monotonic() - started
            if probe.returncode != expected or elapsed >= 3.5:
                raise RuntimeError(f"Registry {check['version']} {state} probe: exit {probe.returncode}, "
                                   f"expected {expected}; elapsed {elapsed:.2f}s")
    finally:
        identifiers = run("docker", "ps", "--all", "--quiet", "--no-trunc",
                          "--filter", "label=" + OWNER_LABEL + "=" + token).stdout.split()
        if any(not re.fullmatch(r"[0-9a-f]{64}", identifier) for identifier in identifiers):
            raise RuntimeError("Invalid owned container ID; cleanup refused")
        if identifiers:
            run("docker", "rm", "--force", "--volumes", *identifiers)
    print(f"PASS registry {check['version']}: image probe handles healthy, HTTP failure, timeout and recovery", flush=True)


def main():
    # A remote Docker daemon cannot access this host's temporary bind mount.
    architecture = run("docker", "info", "--format", "{{.Architecture}}").stdout.strip()
    architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(architecture, architecture)
    if architecture not in {"amd64", "arm64"}:
        raise RuntimeError("Unsupported Docker architecture: " + architecture)
    go = os.environ.get("GO", "go")
    checks = json.loads(run(go, "run", HELPER, "render", timeout=120).stdout)
    with tempfile.TemporaryDirectory(prefix="areg-probes-") as temporary:
        runtime = Path(temporary) / "runtime"
        runtime.mkdir(mode=0o755)
        # The container's non-root user must be able to read these test files,
        # independently of the invoking shell's umask. No credentials are used.
        runtime.chmod(0o755)
        run(go, "build", "-trimpath", "-o", str(runtime / "server"), HELPER, timeout=120,
            env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": architecture})
        (runtime / "server").chmod(0o555)
        (runtime / ".curlrc").write_text("url = http://127.0.0.1:1/config-must-not-be-loaded\n")
        (runtime / "state").write_text("healthy")
        for file in (".curlrc", "state"):
            (runtime / file).chmod(0o644)
        for check in checks:
            check_probe(runtime, check)


if __name__ == "__main__":
    main()
