#!/usr/bin/env python3
"""Vendor or verify the tested public chart without host credential helpers."""
import argparse
import hashlib
import json
from pathlib import Path
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
VERSION = "4.17.2"
DEST = ROOT / f"charts/agentregistry-operator/charts/metacontroller-helm-{VERSION}.tgz"
MANIFEST = "sha256:7227209acbead697ed1bc32991e3e15fa63f3a3ea3f8215ddc67d47de3531c6d"
ARCHIVE = "sha256:10497c22b8a35ca97ad87a06a3f7dd1b7045b7944c79e75a9c1b4427d05b842b"
BASE = "https://ghcr.io/v2/metacontroller/metacontroller-helm/"


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def get(url, headers=None):
    request = urllib.request.Request(url, headers=headers or {})
    with urllib.request.urlopen(request, timeout=60) as response:
        return response.read()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--verify", action="store_true", help="verify the vendored archive offline without downloading")
    args = parser.parse_args()
    if DEST.exists():
        if digest(DEST.read_bytes()) != ARCHIVE:
            raise ValueError("Vendored Metacontroller chart checksum mismatch")
    elif args.verify:
        raise FileNotFoundError(f"Missing vendored chart: {DEST}")
    else:
        token = json.loads(get("https://ghcr.io/token?scope=repository:metacontroller/metacontroller-helm:pull&service=ghcr.io"))["token"]
        headers = {"Authorization": "Bearer " + token,
                   "Accept": "application/vnd.oci.image.manifest.v1+json"}
        manifest = get(BASE + "manifests/" + VERSION, headers)
        if digest(manifest) != MANIFEST:
            raise ValueError("Metacontroller chart manifest digest mismatch")
        layers = json.loads(manifest)["layers"]
        if len(layers) != 1 or layers[0]["digest"] != ARCHIVE:
            raise ValueError("Metacontroller chart layer digest mismatch")
        archive = get(BASE + "blobs/" + ARCHIVE, headers)
        if digest(archive) != ARCHIVE:
            raise ValueError("Metacontroller chart archive checksum mismatch")
        DEST.parent.mkdir(parents=True, exist_ok=True)
        temporary = DEST.with_suffix(".tmp")
        temporary.write_bytes(archive)
        temporary.replace(DEST)
    print(DEST)


if __name__ == "__main__":
    main()
