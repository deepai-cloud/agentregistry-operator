#!/usr/bin/env python3
"""Build distributable artifacts locally; never publish or include private state."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "charts/agentregistry-operator"
# Build metadata is excluded because the same version becomes a container tag.
VERSION_PATTERN = re.compile(
    r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
    r"(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
    r"(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?"
)


def reject_symlinks(directory):
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise SystemExit(f"Refusing to distribute symlink: {path}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", default=re.search(r"(?m)^version: (.+)$", (CHART / "Chart.yaml").read_text())[1])
    parser.add_argument("--image", default="agentregistry-operator:dev")
    args = parser.parse_args()
    if not VERSION_PATTERN.fullmatch(args.version):
        parser.error("--version must be a semantic version without a v prefix or build metadata")
    if "@" in args.image:
        repository, digest = args.image.rsplit("@", 1)
        tag = ""
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            parser.error("--image digest must be sha256 followed by 64 lowercase hexadecimal characters")
    elif ":" in args.image.rsplit("/", 1)[-1]:
        repository, tag = args.image.rsplit(":", 1)
        digest = ""
        if not re.fullmatch(r"[\w][\w.-]{0,127}", tag, re.ASCII):
            parser.error("invalid image tag")
    else:
        parser.error("--image needs an explicit tag or sha256 digest")
    if not re.fullmatch(r"[a-z0-9][a-z0-9._:/-]*", repository):
        parser.error("invalid image repository")
    helm = os.environ.get("HELM", "helm")
    destination = ROOT / "dist"
    destination.mkdir(exist_ok=True)
    prefix = f"agentregistry-operator-{args.version}"
    with tempfile.TemporaryDirectory(prefix="areg-package-") as temporary:
        staging = Path(temporary)
        chart = staging / "chart"
        if CHART.is_symlink():
            raise SystemExit(f"Refusing to distribute symlink: {CHART}")
        shutil.copytree(CHART, chart, symlinks=True)
        reject_symlinks(chart)
        values = (chart / "values.yaml").read_text()
        image = {"repository": repository, "tag": tag, "digest": digest, "pullPolicy": "IfNotPresent"}
        values, count = re.subn(r"(?ms)^image:\n.*?(?=^\S|\Z)", "image: " + json.dumps(image) + "\n\n", values, count=1)
        if count != 1:
            raise SystemExit("Cannot locate the chart's image values block")
        (chart / "values.yaml").write_text(values)
        subprocess.run([helm, "lint", "--strict", str(chart)], check=True)
        subprocess.run([helm, "package", str(chart), "--version", args.version, "--app-version", args.version, "--destination", str(destination)], check=True)
        archive = destination / f"{prefix}.tgz"
        subprocess.run([helm, "template", "package-check", str(archive), "--namespace", "agentregistry-system", "--kube-version", "1.34.0"], check=True, stdout=subprocess.DEVNULL)
        bundle = staging / prefix
        bundle.mkdir()
        # Explicit public inputs prevent private state and build outputs leaking.
        for name in ("LICENSE", "README.md", "Makefile", "Dockerfile", ".dockerignore", ".gitignore", "go.mod", "cmd", "internal", "charts", "scripts", "docs", "examples", ".github/workflows"):
            source = ROOT / name
            if source.is_symlink():
                raise SystemExit(f"Refusing to distribute symlink: {source}")
            if source.is_dir():
                shutil.copytree(source, bundle / name, symlinks=True,
                                ignore=shutil.ignore_patterns("__pycache__", "*.pyc", ".cache", ".local", ".git", ".DS_Store"))
            else:
                shutil.copy2(source, bundle / name)
        shutil.copy2(archive, bundle / archive.name)
        (bundle / "RELEASE.json").write_text(json.dumps({"version": args.version, "image": args.image, "chart": archive.name}, indent=2) + "\n")
        bundle_archive = destination / f"{prefix}-bundle.tar.gz"
        reject_symlinks(bundle)
        with tarfile.open(bundle_archive, "w:gz") as tar:
            tar.add(bundle, arcname=prefix)
        artifacts = [archive, bundle_archive]
        (destination / "SHA256SUMS").write_text("".join(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n" for path in artifacts))
    print(f"Artifacts ready in {destination}; image: {args.image}")
    print("No image, chart, or release has been published.")


if __name__ == "__main__":
    main()
