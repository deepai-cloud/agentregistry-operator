#!/usr/bin/env python3
"""Build distributable artifacts locally; never publish or include private state."""
import argparse
import gzip
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
EXCLUDED_SOURCE_NAMES = ("__pycache__", "*.pyc", ".cache", ".local", ".git", ".DS_Store")
# Build metadata is excluded because the same version becomes a container tag.
VERSION_PATTERN = re.compile(
    r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
    r"(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
    r"(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?"
)


def source_path(relative):
    """Check each component before entering a public input from the checkout."""
    path = ROOT
    for part in Path(relative).parts:
        path = path / part
        if path.is_symlink():
            raise SystemExit(f"Refusing to distribute symlink: {path}")
    return path


def reject_symlinks(directory):
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise SystemExit(f"Refusing to distribute symlink: {path}")


def create_archive(directory, destination, prefix=""):
    """Use stable archive metadata and ordering, without following symlinks."""
    reject_symlinks(directory)

    def normalize(info):
        info.uid = info.gid = 0
        info.uname = info.gname = ""
        info.mtime = 0
        return info

    with destination.open("wb") as output:
        with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w") as tar:
                if prefix:
                    tar.add(directory, arcname=prefix, filter=normalize)
                else:
                    for path in sorted(directory.iterdir()):
                        tar.add(path, arcname=path.name, filter=normalize)


def package_binaries(staging, artifacts, prefix, release):
    for architecture in ("amd64", "arm64"):
        directory = staging / f"linux-{architecture}"
        directory.mkdir()
        environment = {**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": architecture}
        for name in ("operator", "gateway"):
            subprocess.run([
                os.environ.get("GO", "go"), "build", "-trimpath", "-buildvcs=false",
                "-o", str(directory / name), f"./cmd/{name}",
            ], cwd=ROOT, env=environment, check=True)
            (directory / name).chmod(0o755)
        shutil.copy2(ROOT / "LICENSE", directory / "LICENSE")
        (directory / "RELEASE.json").write_text(json.dumps({
            **release, "os": "linux", "architecture": architecture,
            "binaries": ["operator", "gateway"],
        }, indent=2) + "\n")
        archive = artifacts / f"{prefix}-linux-{architecture}.tar.gz"
        create_archive(directory, archive)
        yield archive


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    chart_metadata = source_path("charts/agentregistry-operator/Chart.yaml")
    parser.add_argument("--version", default=re.search(r"(?m)^version: (.+)$", chart_metadata.read_text())[1])
    parser.add_argument("--image", default="agentregistry-operator:dev")
    parser.add_argument("--binaries", action="store_true", help="include Linux amd64 and arm64 operator/gateway archives")
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
    prefix = f"agentregistry-operator-{args.version}"
    with tempfile.TemporaryDirectory(prefix="areg-package-") as temporary:
        staging = Path(temporary)
        output = staging / "artifacts"
        output.mkdir()
        chart = staging / "chart"
        shutil.copytree(chart_metadata.parent, chart, symlinks=True,
                        ignore=shutil.ignore_patterns(*EXCLUDED_SOURCE_NAMES))
        reject_symlinks(chart)
        values = (chart / "values.yaml").read_text()
        image = {"repository": repository, "tag": tag, "digest": digest, "pullPolicy": "IfNotPresent"}
        values, count = re.subn(r"(?ms)^image:\n.*?(?=^\S|\Z)", "image: " + json.dumps(image) + "\n\n", values, count=1)
        if count != 1:
            raise SystemExit("Cannot locate the chart's image values block")
        (chart / "values.yaml").write_text(values)
        subprocess.run([helm, "lint", "--strict", str(chart)], check=True)
        subprocess.run([helm, "package", str(chart), "--version", args.version, "--app-version", args.version, "--destination", str(output)], check=True)
        archive = output / f"{prefix}.tgz"
        subprocess.run([helm, "template", "package-check", str(archive), "--namespace", "agentregistry-system", "--kube-version", "1.34.0"], check=True, stdout=subprocess.DEVNULL)
        bundle = staging / prefix
        bundle.mkdir()
        # Explicit public inputs prevent private state and build outputs leaking.
        for name in ("LICENSE", "README.md", "Makefile", "Dockerfile", ".dockerignore", ".gitignore", "go.mod", "cmd", "internal", "charts", "scripts", "docs", "examples", ".github/workflows"):
            source = source_path(name)
            if source.is_dir():
                shutil.copytree(source, bundle / name, symlinks=True,
                                ignore=shutil.ignore_patterns(*EXCLUDED_SOURCE_NAMES))
            else:
                shutil.copy2(source, bundle / name)
        shutil.copy2(archive, bundle / archive.name)
        release = {"version": args.version, "image": args.image, "chart": archive.name}
        (bundle / "RELEASE.json").write_text(json.dumps(release, indent=2) + "\n")
        bundle_archive = output / f"{prefix}-bundle.tar.gz"
        create_archive(bundle, bundle_archive, prefix=prefix)
        artifacts = [archive, bundle_archive]
        if args.binaries:
            artifacts.extend(package_binaries(staging, output, prefix, release))
        sums = output / "SHA256SUMS"
        sums.write_text("".join(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n" for path in artifacts))
        files = [*artifacts, sums]
        destination.mkdir(exist_ok=True)
        # Stage on the destination filesystem so each rename replaces the
        # directory entry, even when an old output links to another file.
        with tempfile.TemporaryDirectory(dir=destination, prefix=".areg-package-") as pending:
            incoming = Path(pending)
            for path in files:
                target = destination / path.name
                if not target.is_symlink() and target.is_dir():
                    raise SystemExit(f"Release output is a directory: {target}")
                shutil.copy2(path, incoming / path.name)
            for path in files:
                (incoming / path.name).replace(destination / path.name)
    print(f"Artifacts ready in {destination}; image: {args.image}")
    print("No image, chart, or release has been published.")


if __name__ == "__main__":
    main()
