#!/usr/bin/env python3
"""Check acceptance HTTP isolation and recovery; no Docker or cluster access."""

import base64
from contextlib import ExitStack, contextmanager
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import urllib.error
import urllib.request


spec = importlib.util.spec_from_file_location(
    "managed_acceptance", Path(__file__).with_name("test-managed-postgres.py"))
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)


@contextmanager
def server(status=200, body=b'{"servers":[]}', location=None, port=0):
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            requests.append((self.path, self.headers.get("Authorization")))
            response_status = status(len(requests)) if callable(status) else status
            if response_status is None:
                self.close_connection = True  # Drop the connection before any response.
                return
            self.send_response(response_status)
            if location is not None:
                self.send_header("Location", location)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_):
            pass

    httpd = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    worker = threading.Thread(target=httpd.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
    worker.start()
    try:
        yield httpd.server_port, requests
    finally:
        httpd.shutdown()
        httpd.server_close()
        worker.join(timeout=5)


class Clock:
    def __init__(self):
        self.now = 0

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


class AcceptanceHTTPTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="areg-http-test-")
        self.addCleanup(temporary.cleanup)
        self.clock = Clock()
        # Keep these tests independent of the machine's proxy settings and any
        # opener cached by previous urlopen calls. Test credentials are synthetic.
        for replacement in (patch.object(acceptance, "ROOT", Path(temporary.name)),
                            patch.dict(os.environ, {}, clear=True),
                            patch.object(acceptance, "time", self.clock),
                            patch.object(acceptance, "say"),
                            patch.object(urllib.request, "_opener", None)):
            replacement.start()
            self.addCleanup(replacement.stop)

    def runner(self, port):
        runner = acceptance.Acceptance()
        runner.username, runner.password, runner.port = "acceptance-test", "synthetic-password", port
        return runner

    def recovery_runner(self, port):
        runner = self.runner(port)
        # Only Kubernetes/process/SQL boundaries are mocked. Exercise the real
        # restart workflow and its bounded HTTP wait, with no cluster commands.
        runner.db_secret = {"metadata": {"uid": "database-secret"}, "data": {"password": "synthetic"}}
        runner.get = Mock(side_effect=[{"metadata": {"uid": "old-pod"}}, runner.db_secret])
        restarted = {"metadata": {"uid": "new-pod"},
                     "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
        runner.kubectl = Mock(return_value=SimpleNamespace(returncode=0, stdout=json.dumps(restarted)))
        runner.ready = Mock(return_value=True)
        runner.sql = Mock(return_value="survives-restart")
        runner.check = Mock()
        runner.forward = Mock()
        runner.forward.poll.return_value = None
        return runner

    def test_catalog_probe_uses_only_the_port_bound_by_kubectl(self):
        runner = self.runner(0)
        self.addCleanup(runner.cleanup)
        with ExitStack() as listeners:
            def catalog_status(attempt):
                return 401 if attempt == 1 else 200

            owned_port, owned = listeners.enter_context(server(status=catalog_status))
            competing = []

            def launch(args, *, stdout, **kwargs):
                local, remote = args[args.index("service/catalog") + 1].split(":")
                self.assertEqual(remote, "8080")
                self.assertIn("--address=127.0.0.1", args)
                if local:
                    # Another HTTP server wins the bind before kubectl starts.
                    _, requests = listeners.enter_context(server(status=catalog_status, port=int(local)))
                    competing.append(requests)
                else:
                    # kubectl reports the local listener and resolved pod targetPort.
                    stdout.write(f"Forwarding from 127.0.0.1:{owned_port} -> 8443\n")
                    stdout.flush()
                return Mock(poll=Mock(return_value=None))

            with patch.object(acceptance.subprocess, "Popen", side_effect=launch):
                runner.inspect_http()
            self.assertFalse(any(competing), "catalog probe reached a competing listener")
            self.assertEqual(runner.port, owned_port)
            self.assertEqual(len(owned), 2)
            self.assertIsNone(owned[0][1])
            self.assertIsNotNone(owned[1][1])

    def test_forward_must_announce_loopback_before_catalog_requests(self):
        runner = self.runner(0)
        self.addCleanup(runner.cleanup)
        runner.http = Mock()

        def launch(args, *, stdout, **kwargs):
            stdout.write("Forwarding from 0.0.0.0:12345 -> 8443\n"
                         "Forwarding from 127.0.0.1:12345 -> ")
            stdout.flush()
            return Mock(poll=Mock(return_value=None))

        with patch.object(acceptance.subprocess, "Popen", side_effect=launch):
            with self.assertRaisesRegex(RuntimeError, "Timed out: registry port-forward"):
                runner.inspect_http()
        self.assertEqual(self.clock.now, 20)
        runner.http.assert_not_called()

    def test_authenticated_request_ignores_environment_proxy(self):
        with server() as (proxy_port, proxied), server() as (port, direct):
            os.environ.update({"http_proxy": f"http://127.0.0.1:{proxy_port}", "no_proxy": ""})
            runner = self.runner(port)
            status, body = runner.http(True)
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body), {"servers": []})
            self.assertEqual(proxied, [], "local test credentials reached the configured proxy")
            expected = "Basic " + base64.b64encode(b"acceptance-test:synthetic-password").decode()
            self.assertEqual(direct, [("/v0/servers?limit=1", expected)])

    def test_redirects_cannot_forward_credentials_or_fake_success(self):
        for status in (301, 302, 303, 307, 308):
            with self.subTest(status=status), server() as (other_port, redirected):
                # A distinct authority represents a service outside the owned forward.
                location = f"http://localhost:{other_port}/fake-registry"
                with server(status, b"redirect denied", location) as (port, direct):
                    result, body = self.runner(port).http(True)
                    self.assertEqual(result, status, "redirect was mistaken for registry success")
                    self.assertEqual(body, b"redirect denied")
                    self.assertEqual(redirected, [], "redirect target received a credentialed request")
                    self.assertEqual(len(direct), 1)

    def test_anonymous_and_error_responses_remain_observable(self):
        for status in (200, 401, 503):
            with self.subTest(status=status), server(status, b"response") as (port, requests):
                result, body = self.runner(port).http(False)
                self.assertEqual((result, body), (status, b"response"))
                self.assertEqual(requests, [("/v0/servers?limit=1", None)])

    def test_restart_retries_dropped_connection_and_unavailable_response(self):
        with server(status=lambda attempt: (None, 503, 200)[min(attempt - 1, 2)]) as (port, requests):
            runner = self.recovery_runner(port)
            runner.restart_database()
            self.assertEqual(len(requests), 3)
            self.assertTrue(all(auth is not None for _, auth in requests))
            runner.sql.assert_called_once_with("SELECT value FROM acceptance_persistence;")
            runner.check.assert_called_once()

    def test_restart_retries_other_transport_failures(self):
        for error in (urllib.error.URLError("connection refused"), TimeoutError("timed out"),
                      http.client.IncompleteRead(b"partial")):
            with self.subTest(error=type(error).__name__):
                runner = self.recovery_runner(1)
                runner.http = Mock(side_effect=[error, (200, b'{"servers":[]}')])
                runner.restart_database()
                self.assertEqual(runner.http.call_count, 2)
                runner.check.assert_called_once()

    def test_restart_http_recovery_remains_bounded(self):
        runner = self.recovery_runner(1)
        runner.http = Mock(side_effect=urllib.error.URLError("connection refused"))
        with self.assertRaisesRegex(RuntimeError, "Timed out: authenticated API recovers after database restart"):
            runner.restart_database()
        self.assertGreater(runner.http.call_count, 1)
        self.assertEqual(self.clock.now, 240)
        runner.sql.assert_not_called()
        runner.check.assert_not_called()

    def test_exited_forward_fails_before_http_retries(self):
        runner = self.recovery_runner(1)
        runner.forward.poll.return_value = 1
        runner.http = Mock(return_value=(200, b'{"servers":[]}'))
        with self.assertRaisesRegex(RuntimeError, "port-forward"):
            runner.restart_database()
        self.assertEqual(self.clock.now, 0)
        runner.http.assert_not_called()
        runner.check.assert_not_called()

    def test_unexpected_errors_are_not_hidden_by_retries(self):
        runner = self.recovery_runner(1)
        runner.http = Mock(side_effect=ValueError("invalid test configuration"))
        with self.assertRaisesRegex(ValueError, "invalid test configuration"):
            runner.restart_database()
        self.assertEqual(self.clock.now, 0)
        runner.check.assert_not_called()


if __name__ == "__main__":
    unittest.main()
