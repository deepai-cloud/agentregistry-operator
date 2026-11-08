#!/usr/bin/env python3
"""Check release artifacts in temporary checkouts; never publish or use Docker."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
VERSION = "1.2.3-rc.1"
PREFIX = "agentregistry-operator-" + VERSION


class PackageTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="areg-package-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        # A minimal public source tree keeps the test independent of private
        # files in the real checkout. Use the actual packaging code and chart.
        for name in ("LICENSE", "README.md", "Makefile", "Dockerfile", ".dockerignore", ".gitignore", "go.mod"):
            (self.root / name).write_text("public fixture: " + name + "\n")
        for name in ("cmd", "internal", "scripts", "docs", "examples", ".github/workflows"):
            directory = self.root / name
            directory.mkdir(parents=True)
            (directory / "public.txt").write_text("public fixture: " + name + "\n")
        shutil.copy2(ROOT / "scripts/package.py", self.root / "scripts/package.py")
        shutil.copytree(ROOT / "charts/agentregistry-operator", self.root / "charts/agentregistry-operator")
        for name in (".local/credentials.txt", ".cache/token", ".git/config", "bin/operator",
                     "examples/.local/credentials.txt", "scripts/__pycache__/private.pyc",
                     "charts/agentregistry-operator/.local/credentials.txt",
                     "charts/agentregistry-operator/.cache/token",
                     "charts/agentregistry-operator/__pycache__/private.pyc",
                     "charts/agentregistry-operator/files/.local/credentials.txt"):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("private-package-test-sentinel")
        self.dist = self.root / "dist"
        self.dist.mkdir()
        (self.dist / "older-release.tgz").write_bytes(b"older release must survive")
        helm = shutil.which(os.environ.get("HELM", "helm"))
        self.assertIsNotNone(helm, "Helm is required for packaging tests")
        self.helm = str(Path(helm).resolve())

    def package(self, image, *args):
        return subprocess.run([sys.executable, "scripts/package.py", "--version", VERSION,
                               "--image", image, *args], cwd=self.root, text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60,
                              env={**os.environ, "HELM": self.helm, "GO": str(self.root / "fake-go")})

    def check_archives(self, image, binaries=False):
        names = [PREFIX + ".tgz", PREFIX + "-bundle.tar.gz"]
        if binaries:
            names += [PREFIX + "-linux-" + arch + ".tar.gz" for arch in ("amd64", "arm64")]
        sums = (self.dist / "SHA256SUMS").read_text().splitlines()
        self.assertEqual(sums, [hashlib.sha256((self.dist / name).read_bytes()).hexdigest() + "  " + name
                                for name in names], "checksums must cover exactly this run's complete archives")
        self.assertEqual((self.dist / "older-release.tgz").read_bytes(), b"older release must survive")
        with tarfile.open(self.dist / (PREFIX + ".tgz")) as chart:
            for member in chart.getmembers():
                if member.isfile():
                    self.assertNotIn(b"private-package-test-sentinel", chart.extractfile(member).read(),
                                     "private state entered the installable chart")
        with tarfile.open(self.dist / (PREFIX + "-bundle.tar.gz")) as bundle:
            members = bundle.getnames()
            for expected in ("README.md", "LICENSE", "scripts/package.py", "internal/public.txt",
                             ".github/workflows/public.txt", PREFIX + ".tgz"):
                self.assertIn(PREFIX + "/" + expected, members)
            for member in bundle.getmembers():
                self.assertFalse(member.issym() or member.islnk(), "source bundle contains a link")
                if member.isfile():
                    self.assertNotIn(b"private-package-test-sentinel", bundle.extractfile(member).read())
            release = json.load(bundle.extractfile(PREFIX + "/RELEASE.json"))
            self.assertEqual(release, {"version": VERSION, "image": image, "chart": PREFIX + ".tgz"})
            self.assertEqual(bundle.extractfile(PREFIX + "/" + PREFIX + ".tgz").read(),
                             (self.dist / (PREFIX + ".tgz")).read_bytes())
        # Render the actual packaged chart, without extracting archives to disk.
        rendered = subprocess.run([self.helm, "template", "package-test", str(self.dist / (PREFIX + ".tgz")),
                                   "--namespace", "registry-system", "--kube-version", "1.34.0"],
                                  text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30, check=True).stdout
        documents = [json.loads(body) for section in rendered.split("\n---")
                     if (body := "\n".join(line for line in section.splitlines() if not line.startswith("#")).strip()).startswith("{")]
        deployment = next(document for document in documents if document["kind"] == "Deployment")
        container = deployment["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["image"], image)
        self.assertIn({"name": "GATEWAY_IMAGE", "value": image}, container["env"])
        self.assertEqual(deployment["metadata"]["annotations"]["registry.deepai.cloud/operator-version"], VERSION)
        return release

    def test_tagged_chart_and_source_bundle_exclude_private_state(self):
        image = "registry.example.test:5000/team/operator:build-7"
        result = self.package(image)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.check_archives(image)

    def test_existing_output_links_do_not_overwrite_their_targets(self):
        chart = self.dist / (PREFIX + ".tgz")
        original = self.root / ".local/other-file"
        original.write_bytes(b"keep the original file")
        chart.symlink_to(original)
        os.link(self.dist / "older-release.tgz", self.dist / (PREFIX + "-bundle.tar.gz"))
        missing = self.root / ".local/missing"
        (self.dist / "SHA256SUMS").symlink_to(missing)

        image = "registry.example.test/operator:test"
        result = self.package(image)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(original.read_bytes() == b"keep the original file", "overwrote the output symlink's target")
        self.assertFalse(chart.is_symlink())
        self.assertFalse(missing.exists(), "created a dangling output symlink's target")
        self.assertFalse((self.dist / "SHA256SUMS").is_symlink())
        self.check_archives(image)

    def test_directory_at_checksum_path_preserves_existing_release(self):
        chart = self.dist / (PREFIX + ".tgz")
        chart.write_bytes(b"previous chart")
        checksums = self.dist / "SHA256SUMS"
        checksums.mkdir()

        result = self.package("registry.example.test/operator:test")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Release output is a directory", result.stderr)
        self.assertEqual(chart.read_bytes(), b"previous chart")
        self.assertCountEqual(self.dist.iterdir(), [chart, checksums, self.dist / "older-release.tgz"])
        self.assertEqual(list(checksums.iterdir()), [])

    def test_direct_helm_packaging_excludes_chart_local_state(self):
        subprocess.run([self.helm, "package", str(self.root / "charts/agentregistry-operator"),
                        "--version", VERSION, "--destination", str(self.dist)],
                       text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30, check=True)
        with tarfile.open(self.dist / (PREFIX + ".tgz")) as chart:
            self.assertIn("agentregistry-operator/Chart.yaml", chart.getnames())
            self.assertIn("agentregistry-operator/LICENSE", chart.getnames())
            for member in chart.getmembers():
                if member.isfile():
                    self.assertNotIn(b"private-package-test-sentinel", chart.extractfile(member).read(),
                                     "Helm included local state in the chart")

    def test_digest_and_binary_archive_metadata_match_the_release(self):
        # The ordinary CI package build tests the real compiler. This stand-in
        # makes the requested platform and build options visible in each file.
        compiler = self.root / "fake-go"
        compiler.write_text("#!/usr/bin/env python3\nimport json, os, pathlib, sys\n"
                            "output = pathlib.Path(sys.argv[sys.argv.index('-o') + 1])\n"
                            "output.write_text(json.dumps({'os': os.environ['GOOS'], 'arch': os.environ['GOARCH'], "
                            "'cgo': os.environ['CGO_ENABLED'], 'args': sys.argv[1:]}))\n")
        compiler.chmod(0o755)
        image = "registry.example.test/team/operator@sha256:" + "a" * 64
        result = self.package(image, "--binaries")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        release = self.check_archives(image, binaries=True)
        for architecture in ("amd64", "arm64"):
            with self.subTest(architecture=architecture), tarfile.open(self.dist / f"{PREFIX}-linux-{architecture}.tar.gz") as archive:
                self.assertEqual(set(archive.getnames()), {"operator", "gateway", "LICENSE", "RELEASE.json"})
                self.assertEqual(json.load(archive.extractfile("RELEASE.json")), {
                    **release, "os": "linux", "architecture": architecture, "binaries": ["operator", "gateway"],
                })
                for binary in ("operator", "gateway"):
                    self.assertEqual(archive.getmember(binary).mode, 0o755)
                    build = json.load(archive.extractfile(binary))
                    self.assertEqual((build["os"], build["arch"], build["cgo"]), ("linux", architecture, "0"))
                    for flag in ("build", "-trimpath", "-buildvcs=false", "./cmd/" + binary):
                        self.assertIn(flag, build["args"])

    def test_symlink_failure_preserves_existing_release_files(self):
        (self.dist / "SHA256SUMS").write_bytes(b"previous checksums\n")
        before = {path.name: path.read_bytes() for path in self.dist.iterdir()}
        for location in ("charts/agentregistry-operator/private", "examples/private-link"):
            with self.subTest(location=location):
                link = self.root / location
                link.symlink_to(self.root / ".local/credentials.txt")
                try:
                    result = self.package("registry.example.test/operator:test")
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("Refusing to distribute symlink", result.stderr)
                    self.assertNotIn("private-package-test-sentinel", result.stdout + result.stderr)
                    self.assertEqual({path.name: path.read_bytes() for path in self.dist.iterdir()}, before)
                finally:
                    link.unlink()

    def test_source_bundle_refuses_a_symlink_above_workflows(self):
        private = self.root / ".local/shared-github"
        github = self.root / ".github"
        github.rename(private)
        (private / "workflows/private.txt").write_text("private-package-test-sentinel")
        github.symlink_to(private, target_is_directory=True)

        result = self.package("registry.example.test/operator:test")
        self.assertNotEqual(result.returncode, 0, "packaging followed .github into private state")
        self.assertIn("Refusing to distribute symlink: " + str(self.root.resolve() / ".github"), result.stderr)
        self.assertEqual([path.name for path in self.dist.iterdir()], ["older-release.tgz"])


if __name__ == "__main__":
    unittest.main()
