#!/usr/bin/env python3
"""Generate authentication for the tenant-a/catalog demo; no cluster access."""

import argparse
import json
import os
from pathlib import Path
import secrets
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", default=".local/quickstart")
    parser.add_argument("--external-database", action="store_true",
                        help="also generate Secrets for the disposable external PostgreSQL example")
    args = parser.parse_args()
    directory = Path(args.output_dir)
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    paths = [directory / "secrets.json", directory / "credentials.txt", directory / "curl.conf"]
    if any(path.exists() or path.is_symlink() for path in paths):
        parser.error("credentials already exist; reuse them or select a fresh output directory")
    password = secrets.token_hex(24)
    username = "registry-admin"

    def secret(name, data):
        return {"apiVersion": "v1", "kind": "Secret",
                "metadata": {"name": name, "namespace": "tenant-a"},
                "type": "Opaque", "stringData": data}

    objects = [secret("registry-auth", {"username": username, "password": password,
                                        "jwt-key": secrets.token_hex(32)})]
    if args.external_database:
        db_password = secrets.token_hex(24)
        admin_password = secrets.token_hex(24)
        objects.extend([secret("registry-database", {
            "url": "postgres://registry:" + db_password
            + "@postgres.tenant-a.svc.cluster.local:5432/registry?sslmode=disable"}),
        secret("registry-postgres", {
            "postgres-password": admin_password,
            "init.sql": "CREATE ROLE registry LOGIN PASSWORD '" + db_password
            + "' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;\n"
            + "ALTER DATABASE registry OWNER TO registry;\n"
            + "CREATE EXTENSION IF NOT EXISTS pg_trgm;\n"
            + "CREATE EXTENSION IF NOT EXISTS vector;\n"
            + "ALTER SCHEMA public OWNER TO registry;\n"}),
        ])
    payloads = [json.dumps({"apiVersion": "v1", "kind": "List", "items": objects}, indent=2) + "\n",
                "Username: " + username + "\nPassword: " + password + "\n",
                'user = "' + username + ":" + password + '"\n']
    # Write the complete set privately before exposing any file to demo.sh.
    # Hard links publish without overwriting a file created since the check
    # above. Publish secrets.json last: the demo uses it to decide to reuse.
    with tempfile.TemporaryDirectory(dir=directory, prefix=".credentials-") as temporary:
        staging = Path(temporary)
        for path, payload in zip(paths, payloads):
            descriptor = os.open(staging / path.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "w") as stream:
                stream.write(payload)
        try:
            for path in paths[1:] + paths[:1]:
                os.link(staging / path.name, path)
        except BaseException:
            # Remove only this attempt's files, including on Ctrl-C. A file
            # supplied by another writer (or a symlink) must remain untouched.
            for path in paths:
                try:
                    if os.path.samestat(path.lstat(), (staging / path.name).stat()):
                        path.unlink()
                except FileNotFoundError:
                    pass
            raise
    print("Created demo credentials in " + str(directory) + "; keep these files private.")


if __name__ == "__main__":
    main()
