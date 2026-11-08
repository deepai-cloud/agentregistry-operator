#!/usr/bin/env python3
"""Test the generated PostgreSQL scripts with disposable, network-isolated containers.

Requires Docker and Python 3. Uses the image and scripts embedded in database.go,
non-root execution, a read-only root filesystem, and anonymous volumes removed
on completion. No cluster, published port, or existing volume is used.
"""

import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time
import uuid


ROOT = Path(__file__).resolve().parents[1]
SOURCE = (ROOT / "internal/operator/database.go").read_text()
IMAGE = re.search(r'^const postgresImage = "([^"]+)"', SOURCE, re.M)[1]
INIT = re.search(r'^const databaseInit = `([^`]*)`', SOURCE, re.M)[1]
START = re.search(r'^const databaseStart = `([^`]*)`', SOURCE, re.M)[1]
ROLE_STEP = "WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'registry') \\gexec\n"
DATABASE_STEP = "WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'registry') \\gexec\n"
EXTENSIONS_STEP = "CREATE EXTENSION IF NOT EXISTS vector;\n"
OWNER_LABEL = "registry.deepai.cloud/bootstrap-owner"


def docker(*args, check=True, timeout=60, **kwargs):
    result = subprocess.run(["docker", *args], text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout, **kwargs)
    if check and result.returncode:
        # Do not include SQL output or container logs, which can contain secrets.
        raise RuntimeError(f"Docker {args[0]} failed (exit {result.returncode})")
    return result


def wait(message, predicate, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.25)
    raise RuntimeError("Timed out: " + message)


class Database:
    def __init__(self, work, init):
        self.name = "areg-bootstrap-" + uuid.uuid4().hex[:12]
        self.owner = secrets.token_hex(32)
        self.creation_attempted = False
        self.work = work
        self.init = init

    def __enter__(self):
        try:
            env = self.work / "database.env"
            # Include SQL quoting characters; credentials must survive literal quoting.
            env.write_text("PGDATA=/var/lib/postgresql/data/pgdata\nPOSTGRES_USER=postgres\n"
                           "POSTGRES_DB=postgres\nPOSTGRES_INITDB_ARGS=--auth-host=scram-sha-256\n"
                           "POSTGRES_PASSWORD=" + secrets.token_hex(32) + "\n"
                           "REGISTRY_PASSWORD=" + secrets.token_hex(32) + "'\\quoted\n")
            env.chmod(0o600)
            # The daemon may create the container even if its client times out
            # or is interrupted before returning a successful response.
            self.creation_attempted = True
            docker("create", "--name", self.name, "--label", OWNER_LABEL + "=" + self.owner,
                   "--network", "none", "--user", "999:999",
                   "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                   "--env-file", str(env), "--volume", "/var/lib/postgresql/data",
                   "--volume", "/docker-entrypoint-initdb.d",
                   "--tmpfs", "/var/run/postgresql:uid=999,gid=999,mode=3775",
                   "--tmpfs", "/tmp:mode=1777", "--entrypoint", "/bin/bash",
                   IMAGE, "/docker-entrypoint-initdb.d/start-postgres", "postgres", timeout=180)
            self.copy_script("start-postgres", START)
            self.copy_script("01-registry.sh", self.init)
            docker("start", self.name)
            return self
        except BaseException:
            self.__exit__(None, None, None)
            raise

    def __exit__(self, *_):
        if self.creation_attempted:
            # Match this attempt's random label, including stopped containers.
            # Delete immutable IDs so a reused name cannot target another run.
            containers = docker("ps", "--all", "--quiet", "--no-trunc",
                                "--filter", "label=" + OWNER_LABEL + "=" + self.owner).stdout.split()
            if any(not re.fullmatch(r"[0-9a-f]{64}", identifier) for identifier in containers):
                raise RuntimeError("Docker returned an invalid bootstrap container ID; cleanup refused")
            if containers:
                docker("rm", "--force", "--volumes", *containers)

    def copy_script(self, name, content):
        path = self.work / name
        if path.exists():
            path.chmod(0o600)
        path.write_text(content)
        path.chmod(0o444)
        docker("cp", str(path), self.name + ":/docker-entrypoint-initdb.d/" + name)

    def exec(self, *args, check=True):
        return docker("exec", self.name, *args, check=check)

    def sql(self, sql, database="postgres"):
        return docker("exec", "-i", self.name, "psql", "-XAt", "-U", "postgres", "-d", database,
                      "-v", "ON_ERROR_STOP=1", input=sql).stdout.strip()

    def password_hash(self):
        return self.sql("SELECT md5(rolpassword) FROM pg_authid WHERE rolname = 'registry';")

    def ready(self):
        result = self.exec("bash", "-c", 'test -f "$PGDATA/.registry-initialized" && '
                           'PGPASSWORD="$REGISTRY_PASSWORD" psql -XAtw -h 127.0.0.1 '
                           '-U registry -d registry -c "SELECT 1"', check=False)
        return result.returncode == 0 and result.stdout.strip() == "1"

    def check_setup(self):
        wait("database accepts the original registry password", self.ready)
        if self.sql("SELECT rolcanlogin AND NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication) "
                    "FROM pg_roles WHERE rolname = 'registry';") != "t":
            raise RuntimeError("Registry role has incorrect privileges")
        if self.sql("SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'registry';") != "registry":
            raise RuntimeError("Registry does not own its database")
        if self.sql("SELECT count(*) FROM pg_database, LATERAL aclexplode(datacl) "
                    "WHERE datname = 'registry' AND grantee = 0;") != "0":
            raise RuntimeError("Public database privileges were not revoked")
        if self.sql("SELECT count(*) FROM pg_extension WHERE extname IN ('pg_trgm', 'vector');", "registry") != "2":
            raise RuntimeError("Registry extensions are missing")

    def save_data(self):
        self.sql("CREATE TABLE bootstrap_persistence(value text); "
                 "INSERT INTO bootstrap_persistence VALUES ('survives-bootstrap');", "registry")

    def check_data(self):
        if self.sql("SELECT value FROM bootstrap_persistence;", "registry") != "survives-bootstrap":
            raise RuntimeError("Bootstrap changed existing database contents")


def interrupted(work, stage, step):
    if INIT.count(step) != 1:
        raise RuntimeError("Fault injection must match exactly one bootstrap step: " + stage)
    paused = INIT.replace(step, step + "\\! touch /tmp/bootstrap-paused\nSELECT pg_sleep(300);\n")
    with Database(work, paused) as db:
        wait("bootstrap reaches " + stage, lambda: db.exec("test", "-f", "/tmp/bootstrap-paused", check=False).returncode == 0)
        if db.exec("pg_isready", "-h", "127.0.0.1", check=False).returncode == 0:
            raise RuntimeError("Incomplete bootstrap opened its network listener")
        original_hash = db.password_hash()
        if not original_hash:
            raise RuntimeError("Fault injection did not leave a persisted registry role")
        if stage != "role creation":
            db.save_data()
        docker("kill", "--signal", "KILL", db.name)
        docker("wait", db.name)
        db.copy_script("01-registry.sh", INIT)
        docker("start", db.name)
        db.check_setup()
        if db.password_hash() != original_hash:
            raise RuntimeError("Bootstrap reset an existing role password")
        if stage != "role creation":
            db.check_data()
    print("PASS resumes a hard interruption after " + stage, flush=True)


def sql_failure(work):
    failed = INIT.replace(ROLE_STEP, ROLE_STEP + "SELECT 1 / 0;\n")
    with Database(work, failed) as db:
        if docker("wait", db.name).stdout.strip() == "0":
            raise RuntimeError("Bootstrap ignored a SQL error")
        db.copy_script("01-registry.sh", INIT)
        docker("start", db.name)
        db.check_setup()
    print("PASS retries bootstrap after a SQL failure", flush=True)


def completed_and_legacy(work):
    with Database(work, INIT) as db:
        db.check_setup()
        db.save_data()
        original_hash = db.password_hash()
        # Volumes created by the earlier operator have no completion marker.
        db.exec("bash", "-c", 'rm "$PGDATA/.registry-initialized"')
        docker("restart", db.name)
        db.check_setup()
        db.check_data()
        if db.password_hash() != original_hash:
            raise RuntimeError("Legacy volume upgrade reset its role password")
        # A completed volume must use ordinary PostgreSQL startup, without hooks.
        db.copy_script("01-registry.sh", "#!/bin/bash\nexit 99\n")
        docker("restart", db.name)
        db.check_setup()
        db.check_data()
    print("PASS fresh setup, legacy volume adoption, and completed-volume restarts preserve data and credentials", flush=True)


def main():
    os.umask(0o077)
    docker("version", "--format", "{{.Server.Version}}")
    with tempfile.TemporaryDirectory(prefix="areg-bootstrap-") as temporary:
        work = Path(temporary)
        completed_and_legacy(work)
        for stage, step in [("role creation", ROLE_STEP), ("database creation", DATABASE_STEP), ("extension installation", EXTENSIONS_STEP)]:
            interrupted(work, stage, step)
        sql_failure(work)


if __name__ == "__main__":
    main()
