import copy
import contextlib
import datetime
import errno
import io
import json
import os
import socket
import tempfile
import unittest
from unittest.mock import MagicMock, patch

from check_nacos_native_topology import (PinnedHTTPSConnection, backend_targets, check_probe_dns, evidence, main, probe,
                                         probe_closed_targets, probe_denied,
                                         source_ips, validate, verify_cluster, verify_pair, write_evidence)


class NativeTopologyTest(unittest.TestCase):
    @staticmethod
    def certificates(config, address):
        return {target: {address: "a" * 64}
                for target in (config["tlsHTTP"], config["tlsGRPC"])}

    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        ca_path = os.path.join(directory.name, "ca.pem")
        with open(ca_path, "wb") as stream:
            stream.write(b"test CA bundle")
        self.config = {
            "mode": "tls-l4-per-client-tcp", "nativeAuth": True,
            "backendConnectionReuse": False, "backendHTTP": "127.0.0.1:18848",
            "backendGRPC": "127.0.0.1:19848", "tlsHTTP": "nacos.example:8848",
            "tlsGRPC": "nacos.example:9848", "caFile": ca_path,
            "allowedSources": ["10.42.0.0/16"],
            "backendExternalHost": "10.42.1.10",
        }

    def test_supported_declaration(self):
        self.assertEqual(validate(self.config), [])

    def test_live_probe_requires_two_distinct_tenant_namespaces(self):
        with tempfile.TemporaryDirectory() as directory:
            config_path = os.path.join(directory, "deployment.json")
            with open(config_path, "w", encoding="utf-8") as stream:
                json.dump(self.config, stream)
            for namespaces in ([], ["namespace-a"], ["namespace-a", "namespace-a"]):
                with self.subTest(namespaces=namespaces):
                    args = ["check_nacos_native_topology.py", config_path, "--probe",
                            "--grpc-probe-bin", "/unused/probe",
                            "--login-token-file", "/unused/token-a",
                            "--login-token-file", "/unused/token-b"]
                    for namespace in namespaces:
                        args.extend(("--tenant-namespace", namespace))
                    with patch("sys.argv", args), contextlib.redirect_stderr(io.StringIO()) as errors:
                        with self.assertRaises(SystemExit) as result:
                            main()
                    self.assertEqual(result.exception.code, 2)
                    self.assertIn("two distinct --tenant-namespace", errors.getvalue())

    def test_allowed_sources_cannot_split_full_address_space(self):
        for sources in (
            ["0.0.0.0/1", "128.0.0.0/1"],
            ["::/1", "8000::/1"],
            ["10.42.0.0/16", "0.0.0.0/1", "128.0.0.0/1"],
        ):
            with self.subTest(sources=sources):
                config = copy.deepcopy(self.config)
                config["allowedSources"] = sources
                self.assertTrue(any("entire IPv" in error for error in validate(config)))

        config = copy.deepcopy(self.config)
        config["allowedSources"] = ["10.42.0.0/17", "10.42.128.0/17"]
        self.assertEqual(validate(config), [])

    def test_evidence_writer_never_replaces_existing_record(self):
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "probe.json")
            first = {"probeHost": "first"}
            write_evidence(path, first)
            with self.assertRaises(FileExistsError):
                write_evidence(path, {"probeHost": "second"})
            with open(path, encoding="utf-8") as stream:
                self.assertEqual(json.load(stream), first)
            self.assertEqual(os.listdir(directory), ["probe.json"])

    def test_evidence_writer_rejects_file_created_during_publish(self):
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "probe.json")
            real_link = os.link

            def competing_writer(source, target):
                with open(target, "x", encoding="utf-8") as stream:
                    stream.write("other probe\n")
                return real_link(source, target)

            with patch("check_nacos_native_topology.os.link", side_effect=competing_writer):
                with self.assertRaises(FileExistsError):
                    write_evidence(path, {"probeHost": "late"})
            with open(path, encoding="utf-8") as stream:
                self.assertEqual(stream.read(), "other probe\n")
            self.assertEqual(os.listdir(directory), ["probe.json"])

    def test_unsafe_topologies_fail_closed(self):
        for field, value in (
            ("mode", "l7-http2"), ("nativeAuth", False),
            ("backendConnectionReuse", True), ("backendGRPC", "0.0.0.0:19848"),
            ("backendGRPC", "127.0.0.2:18848"),
            ("allowedSources", ["0.0.0.0/0"]), ("caFile", ""),
            ("backendExternalHost", "0.0.0.0"),
            ("backendExternalHost", "127.0.0.1"),
            ("backendExternalHost", "::ffff:127.0.0.1"),
            ("backendExternalHost", "not-an-ip"),
            ("tlsHTTP", "127.0.0.1:8848"),
            ("tlsHTTP", "localhost:8848"),
            ("tlsHTTP", "api.localhost.:8848"),
            ("tlsHTTP", "*:8848"),
            ("tlsGRPC", "0.0.0.0:9848"),
            ("tlsGRPC", "[::1]:9848"),
            ("tlsGRPC", "[::ffff:127.0.0.1]:9848"),
            ("tlsGRPC", "[fe80::1]:9848"),
            ("tlsGRPC", "nacos-grpc.example:8848"),
        ):
            with self.subTest(field=field):
                config = copy.deepcopy(self.config)
                config[field] = value
                self.assertTrue(validate(config))

    def test_allowed_probe_checks_every_resolved_ip_and_rejects_redirect(self):
        resolved = {"nacos.example:9848": ["10.42.1.10", "10.42.1.11"],
                    "nacos.example:8848": ["10.42.1.10", "10.42.1.11"]}
        context = MagicMock()
        context.wrap_socket.return_value.__enter__.return_value.selected_alpn_protocol.return_value = "h2"
        context.wrap_socket.return_value.__enter__.return_value.getpeercert.return_value = b"grpc certificate"
        connection = MagicMock()
        connection.sock.getpeercert.return_value = b"http certificate"
        with patch("check_nacos_native_topology.ssl.create_default_context", return_value=context), \
             patch("check_nacos_native_topology.socket.create_connection") as connect, \
             patch("check_nacos_native_topology.PinnedHTTPSConnection", return_value=connection) as https:
            def replies(statuses):
                tokens = iter(("tenant-a-token", "tenant-b-token") * 2)
                result = []
                for status in statuses:
                    reply = MagicMock(status=status)
                    if status == 200:
                        reply.read.return_value = json.dumps({"accessToken": next(tokens), "tokenTtl": 3600}).encode()
                    result.append(reply)
                return result

            connection.getresponse.side_effect = replies((404, 401, 200, 404, 200, 404) * 2)
            probe(self.config, resolved, ("tenant-a-token", "tenant-b-token"), ("namespace-a", "namespace-b"))
            self.assertEqual([call.args[2] for call in https.call_args_list],
                             ["10.42.1.10"] * 6 + ["10.42.1.11"] * 6)
            self.assertEqual([call.kwargs["headers"].get("accessToken") for call in connection.request.call_args_list],
                             [None, None, None, "tenant-a-token", None, "tenant-b-token"] * 2)
            self.assertEqual([connection.request.call_args_list[index].args[1] for index in (3, 5, 9, 11)],
                             ["/nacos/v1/cs/configs?tenant=namespace-a&group=GOLAB_FLAGS&dataId=golab.manifest",
                              "/nacos/v1/cs/configs?tenant=namespace-b&group=GOLAB_FLAGS&dataId=golab.manifest"] * 2)
            self.assertEqual([call.args[0] for call in connect.call_args_list],
                             [("10.42.1.10", 9848), ("10.42.1.11", 9848)])

            connection.getresponse.side_effect = replies((404, 401, 200, 404, 200, 404,
                                                          404, 401, 200, 404, 200, 302))
            with self.assertRaisesRegex(ValueError, "10.42.1.11:8848.*HTTP 302"):
                probe(self.config, resolved, ("tenant-a-token", "tenant-b-token"), ("namespace-a", "namespace-b"))

    def test_allowed_probe_rejects_authenticated_http_config_bypass(self):
        resolved = {"nacos.example:9848": ["10.42.1.10"],
                    "nacos.example:8848": ["10.42.1.10"]}
        context = MagicMock()
        grpc = context.wrap_socket.return_value.__enter__.return_value
        grpc.selected_alpn_protocol.return_value = "h2"
        grpc.getpeercert.return_value = b"grpc certificate"
        connection = MagicMock()
        connection.sock.getpeercert.return_value = b"http certificate"
        replies = [MagicMock(status=status) for status in (404, 401, 200, 200)]
        replies[2].read.return_value = b'{"accessToken":"tenant-a-token","tokenTtl":3600}'
        connection.getresponse.side_effect = replies
        with patch("check_nacos_native_topology.ssl.create_default_context", return_value=context), \
             patch("check_nacos_native_topology.socket.create_connection"), \
             patch("check_nacos_native_topology.PinnedHTTPSConnection", return_value=connection):
            with self.assertRaisesRegex(ValueError, "HTTP 200, expected 404"):
                probe(self.config, resolved, ("tenant-a-token", "tenant-b-token"), ("namespace-a", "namespace-b"))

    def test_allowed_probe_rejects_false_success_login_body(self):
        resolved = {"nacos.example:9848": ["10.42.1.10"],
                    "nacos.example:8848": ["10.42.1.10"]}
        context = MagicMock()
        context.wrap_socket.return_value.__enter__.return_value.selected_alpn_protocol.return_value = "h2"
        context.wrap_socket.return_value.__enter__.return_value.getpeercert.return_value = b"grpc certificate"
        for body in (b"<html>OK</html>", b'{}',
                     b'{"accessToken":"wrong","tokenTtl":3600}',
                     b'{"accessToken":"tenant-a-token","tokenTtl":0}',
                     b'{"accessToken":"tenant-a-token","tokenTtl":true}'):
            with self.subTest(body=body):
                connection = MagicMock()
                connection.sock.getpeercert.return_value = b"http certificate"
                replies = [MagicMock(status=status) for status in (404, 401, 200)]
                replies[-1].read.return_value = body
                connection.getresponse.side_effect = replies
                with patch("check_nacos_native_topology.ssl.create_default_context", return_value=context), \
                     patch("check_nacos_native_topology.socket.create_connection"), \
                     patch("check_nacos_native_topology.PinnedHTTPSConnection", return_value=connection):
                    with self.assertRaisesRegex(ValueError, "invalid SDK login response"):
                        probe(self.config, resolved, ("tenant-a-token", "tenant-b-token"), ("namespace-a", "namespace-b"))

    def test_allowed_probe_rejects_invalid_valid_login(self):
        resolved = {"nacos.example:9848": ["10.42.1.10"],
                    "nacos.example:8848": ["10.42.1.10"]}
        context = MagicMock()
        context.wrap_socket.return_value.__enter__.return_value.selected_alpn_protocol.return_value = "h2"
        context.wrap_socket.return_value.__enter__.return_value.getpeercert.return_value = b"grpc certificate"
        connection = MagicMock()
        connection.sock.getpeercert.return_value = b"http certificate"
        replies = [MagicMock(status=status) for status in (404, 401, 200, 404, 401)]
        replies[2].read.return_value = b'{"accessToken":"tenant-a-token","tokenTtl":3600}'
        connection.getresponse.side_effect = replies
        with patch("check_nacos_native_topology.ssl.create_default_context", return_value=context), \
             patch("check_nacos_native_topology.socket.create_connection"), \
             patch("check_nacos_native_topology.PinnedHTTPSConnection", return_value=connection):
            with self.assertRaisesRegex(ValueError, "HTTP 401, expected 200"):
                probe(self.config, resolved, ("tenant-a-token", "tenant-b-token"), ("namespace-a", "namespace-b"))

    def test_allowed_probe_rejects_http_certificate_rotation(self):
        resolved = {"nacos.example:9848": ["10.42.1.10"],
                    "nacos.example:8848": ["10.42.1.10"]}
        context = MagicMock()
        grpc = context.wrap_socket.return_value.__enter__.return_value
        grpc.selected_alpn_protocol.return_value = "h2"
        grpc.getpeercert.return_value = b"grpc certificate"
        connection = MagicMock()
        connection.sock.getpeercert.side_effect = (b"first", b"rotated")
        connection.getresponse.return_value.status = 404
        with patch("check_nacos_native_topology.ssl.create_default_context", return_value=context), \
             patch("check_nacos_native_topology.socket.create_connection"), \
             patch("check_nacos_native_topology.PinnedHTTPSConnection", return_value=connection):
            with self.assertRaisesRegex(ValueError, "certificate changed during probe"):
                probe(self.config, resolved, ("tenant-a-token", "tenant-b-token"), ("namespace-a", "namespace-b"))

    def test_pinned_https_connection_uses_ip_and_cert_hostname(self):
        context = MagicMock()
        context.wrap_socket.return_value.selected_alpn_protocol.return_value = "http/1.1"
        with patch("check_nacos_native_topology.socket.create_connection") as connect:
            connection = PinnedHTTPSConnection("nacos.example", 8848, "10.42.1.11", context)
            connection.connect()
        connect.assert_called_once_with(("10.42.1.11", 8848), timeout=5)
        context.wrap_socket.assert_called_once_with(connect.return_value, server_hostname="nacos.example")

    def test_pinned_https_connection_requires_http11_alpn(self):
        for negotiated in (None, "h2"):
            with self.subTest(negotiated=negotiated):
                context = MagicMock()
                context.wrap_socket.return_value.selected_alpn_protocol.return_value = negotiated
                with patch("check_nacos_native_topology.socket.create_connection"):
                    connection = PinnedHTTPSConnection("nacos.example", 8848, "10.42.1.11", context)
                    with self.assertRaisesRegex(ValueError, "did not negotiate http/1.1"):
                        connection.connect()
                context.wrap_socket.return_value.close.assert_called_once()

    def test_denied_source_checks_both_entry_and_backend_ports(self):
        targets = []

        def refuse(target, timeout):
            targets.append(target)
            raise ConnectionRefusedError()

        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=[(None, None, None, None, ("10.42.1.10", 8848))]):
            with patch("check_nacos_native_topology.socket.create_connection", side_effect=refuse):
                probe_denied(self.config)
        self.assertEqual(targets, [
            ("10.42.1.10", 8848), ("10.42.1.10", 9848),
            ("10.42.1.10", 18848), ("10.42.1.10", 19848),
        ])
        self.assertEqual(evidence(self.config, "denied")["targets"], [
            "nacos.example:8848", "nacos.example:9848",
            "10.42.1.10:18848", "10.42.1.10:19848",
        ])

    def test_allowed_source_must_not_reach_plaintext_backend(self):
        targets = backend_targets(self.config)
        self.assertEqual(targets, [("10.42.1.10", 18848), ("10.42.1.10", 19848)])
        addresses = [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.42.1.10", 0))]

        class Connected:
            def __enter__(self):
                return self

            def __exit__(self, *_):
                return False

        def connect(target, timeout):
            if target[1] == 19848:
                return Connected()
            raise ConnectionRefusedError()

        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=addresses):
            with patch("check_nacos_native_topology.socket.create_connection", side_effect=connect):
                with self.assertRaisesRegex(ValueError, "10.42.1.10:19848"):
                    probe_closed_targets(targets)

    def test_allowed_backend_refusal_uses_routed_target_address(self):
        target = backend_targets(self.config)[0]
        addresses = [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.42.1.11", target[1]))]
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=addresses), \
             patch("check_nacos_native_topology.socket.create_connection") as connect:
            with self.assertRaisesRegex(ValueError, "DNS addresses changed during TCP refusal probe"):
                probe_closed_targets([target], {"10.42.1.10:18848": ["10.42.1.10"]})
        connect.assert_not_called()

    def test_allowed_probe_fails_when_plaintext_backend_is_open(self):
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "deployment.json")
            with open(path, "w", encoding="utf-8") as stream:
                json.dump(self.config, stream)
            token_paths = [os.path.join(directory, f"token-{index}") for index in range(2)]
            for index, token_path in enumerate(token_paths):
                with open(token_path, "w", encoding="utf-8") as stream:
                    stream.write(f"token-{index}\n")
            with patch("sys.argv", ["check_nacos_native_topology.py", path, "--probe",
                                    "--grpc-probe-bin", "/tmp/nacos-native-grpc-probe",
                                    "--tenant-namespace", "namespace-a", "--tenant-namespace", "namespace-b",
                                    "--login-token-file", token_paths[0],
                                    "--login-token-file", token_paths[1]]), \
                 patch("check_nacos_native_topology.source_ips",
                       side_effect=lambda _c, _m, output: output.update({self.config["tlsGRPC"]: ["10.42.1.10"]}) or {}), \
                 patch("check_nacos_native_topology.probe"), \
                 patch("check_nacos_native_topology.subprocess.run",
                       return_value=MagicMock(returncode=0, stdout="PASS: anonymous gRPC denied; two tenant reads and subscriptions allowed; cross-tenant reads and subscriptions denied")), \
                 patch("check_nacos_native_topology.check_probe_dns"), \
                 patch("check_nacos_native_topology.probe_closed_targets",
                       side_effect=ValueError("allowed source reached plaintext backend")) as closed:
                with contextlib.redirect_stderr(io.StringIO()) as errors:
                    self.assertEqual(main(), 1)
            closed.assert_called_once_with(backend_targets(self.config),
                                           {self.config["tlsGRPC"]: ["10.42.1.10"]},
                                           label="allowed source reached plaintext backend")
            self.assertIn("ALERT: live probe failed", errors.getvalue())

    def test_allowed_probe_requires_distinct_tokens_and_omits_them_from_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            config_path = os.path.join(directory, "deployment.json")
            output_path = os.path.join(directory, "evidence.json")
            with open(config_path, "w", encoding="utf-8") as stream:
                json.dump(self.config, stream)
            token_paths = [os.path.join(directory, f"token-{index}") for index in range(2)]
            for token_path in token_paths:
                with open(token_path, "w", encoding="utf-8") as stream:
                    stream.write("same-secret\n")
            arguments = ["check_nacos_native_topology.py", config_path, "--probe",
                         "--grpc-probe-bin", "/tmp/nacos-native-grpc-probe",
                         "--tenant-namespace", "namespace-a", "--tenant-namespace", "namespace-b",
                         "--login-token-file", token_paths[0],
                         "--login-token-file", token_paths[1], "--evidence", output_path]
            with patch("sys.argv", arguments), contextlib.redirect_stderr(io.StringIO()) as errors:
                self.assertEqual(main(), 1)
            self.assertIn("two distinct SDK login tokens", errors.getvalue())
            self.assertFalse(os.path.exists(output_path))

            with open(token_paths[1], "w", encoding="utf-8") as stream:
                stream.write("other-secret\n")
            targets = evidence(self.config, "allowed")["targets"]
            sources = {target: "10.42.0.7" for target in targets}
            resolved = {target: ["10.42.1.10"] for target in targets}
            def routed_sources(_config, _mode, output):
                output.update(resolved)
                return sources

            with patch("sys.argv", arguments), \
                 patch("check_nacos_native_topology.source_ips", side_effect=routed_sources), \
                 patch("check_nacos_native_topology.probe",
                       return_value=self.certificates(self.config, "10.42.1.10")) as live_probe, \
                 patch("check_nacos_native_topology.subprocess.run",
                       return_value=MagicMock(returncode=0, stdout="PASS: anonymous gRPC denied; two tenant reads and subscriptions allowed; cross-tenant reads and subscriptions denied")) as grpc_probe, \
                 patch("check_nacos_native_topology.check_probe_dns"), \
                 patch("check_nacos_native_topology.probe_closed_targets"):
                self.assertEqual(main(), 0)
            live_probe.assert_called_once_with(self.config, resolved, ["same-secret", "other-secret"],
                                               ["namespace-a", "namespace-b"])
            grpc_probe.assert_called_once()
            self.assertEqual(grpc_probe.call_args.args[0][2], "10.42.1.10:9848")
            self.assertEqual(grpc_probe.call_args.args[0][-8:],
                             ["-tenant-a-namespace", "namespace-a", "-tenant-b-namespace", "namespace-b",
                              "-tenant-a-token-file", token_paths[0], "-tenant-b-token-file", token_paths[1]])
            with open(output_path, encoding="utf-8") as stream:
                raw = stream.read()
            self.assertNotIn("same-secret", raw)
            self.assertNotIn("other-secret", raw)
            self.assertEqual(json.loads(raw)["validLoginCount"], 2)
            self.assertTrue(json.loads(raw)["anonymousGRPCDenied"])
            self.assertTrue(json.loads(raw)["anonymousGRPCSubscriptionDenied"])
            self.assertTrue(json.loads(raw)["tenantGRPCIsolation"])
            self.assertEqual(json.loads(raw)["tenantNamespaces"], ["namespace-a", "namespace-b"])
            self.assertEqual(len(json.loads(raw)["caSHA256"]), 64)
            self.assertEqual(json.loads(raw)["tlsCertSHA256"],
                             self.certificates(self.config, "10.42.1.10"))

    def test_allowed_probe_fails_when_anonymous_grpc_read_succeeds(self):
        with tempfile.TemporaryDirectory() as directory:
            config_path = os.path.join(directory, "deployment.json")
            output_path = os.path.join(directory, "evidence.json")
            with open(config_path, "w", encoding="utf-8") as stream:
                json.dump(self.config, stream)
            tokens = [os.path.join(directory, f"token-{index}") for index in range(2)]
            for index, path in enumerate(tokens):
                with open(path, "w", encoding="utf-8") as stream:
                    stream.write(f"token-{index}\n")
            resolved = {target: ["10.42.1.10"] for target in evidence(self.config, "allowed")["targets"]}

            def routed_sources(_config, _mode, output):
                output.update(resolved)
                return {target: "10.42.0.7" for target in resolved}

            with patch("sys.argv", ["check_nacos_native_topology.py", config_path, "--probe",
                                    "--grpc-probe-bin", "/tmp/nacos-native-grpc-probe",
                                    "--tenant-namespace", "namespace-a", "--tenant-namespace", "namespace-b",
                                    "--login-token-file", tokens[0], "--login-token-file", tokens[1],
                                    "--evidence", output_path]), \
                 patch("check_nacos_native_topology.source_ips", side_effect=routed_sources), \
                 patch("check_nacos_native_topology.probe", return_value={}), \
                 patch("check_nacos_native_topology.subprocess.run",
                       return_value=MagicMock(returncode=1, stdout="")), \
                 contextlib.redirect_stderr(io.StringIO()) as errors:
                self.assertEqual(main(), 1)
            self.assertIn("two-tenant gRPC isolation failed", errors.getvalue())
            self.assertFalse(os.path.exists(output_path))

    def test_probe_rejects_ca_change_during_checks(self):
        with tempfile.TemporaryDirectory() as directory:
            config_path = os.path.join(directory, "deployment.json")
            with open(config_path, "w", encoding="utf-8") as stream:
                json.dump(self.config, stream)
            token_paths = [os.path.join(directory, f"token-{index}") for index in range(2)]
            for index, path in enumerate(token_paths):
                with open(path, "w", encoding="utf-8") as stream:
                    stream.write(f"token-{index}\n")

            def change_ca(*_args, **_kwargs):
                with open(self.config["caFile"], "wb") as stream:
                    stream.write(b"rotated CA bundle")

            with patch("sys.argv", ["check_nacos_native_topology.py", config_path, "--probe",
                                    "--grpc-probe-bin", "/tmp/nacos-native-grpc-probe",
                                    "--tenant-namespace", "namespace-a", "--tenant-namespace", "namespace-b",
                                    "--login-token-file", token_paths[0],
                                    "--login-token-file", token_paths[1]]), \
                 patch("check_nacos_native_topology.source_ips",
                       side_effect=lambda _c, _m, output: output.update({self.config["tlsGRPC"]: ["10.42.1.10"]}) or {}), \
                 patch("check_nacos_native_topology.probe", side_effect=change_ca), \
                 patch("check_nacos_native_topology.subprocess.run",
                       return_value=MagicMock(returncode=0, stdout="PASS: anonymous gRPC denied; two tenant reads and subscriptions allowed; cross-tenant reads and subscriptions denied")), \
                 patch("check_nacos_native_topology.check_probe_dns"), \
                 patch("check_nacos_native_topology.probe_closed_targets"), \
                 contextlib.redirect_stderr(io.StringIO()) as errors:
                self.assertEqual(main(), 1)
            self.assertIn("CA file changed during allowed probe", errors.getvalue())

    def test_ipv6_backend_evidence_uses_probe_target_keys(self):
        config = copy.deepcopy(self.config)
        config["backendExternalHost"] = "2001:db8::10"
        self.assertEqual(evidence(config, "denied")["targets"][-2:], [
            "[2001:db8::10]:18848", "[2001:db8::10]:19848",
        ])

    def test_denied_source_fails_if_any_port_accepts_tcp(self):
        class Connected:
            def __enter__(self):
                return self

            def __exit__(self, *_):
                return False

        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=[(None, None, None, None, ("10.42.1.10", 8848))]):
            with patch("check_nacos_native_topology.socket.create_connection", return_value=Connected()):
                with self.assertRaisesRegex(ValueError, "unauthorized source connected"):
                    probe_denied(self.config)

    def test_denied_source_checks_every_dns_address(self):
        addresses = [
            (socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.42.1.10", 8848)),
            (socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.42.1.11", 8848)),
        ]
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=addresses):
            def connect(target, timeout):
                if target[0] == "10.42.1.11":
                    class Connected:
                        def __enter__(self):
                            return self

                        def __exit__(self, *_):
                            return False

                    return Connected()
                raise ConnectionRefusedError()

            with patch("check_nacos_native_topology.socket.create_connection", side_effect=connect):
                with self.assertRaisesRegex(ValueError, "10.42.1.11"):
                    probe_denied(self.config)

    def test_denied_source_rejects_dns_change_after_route_check(self):
        target = self.config["tlsHTTP"]
        addresses = [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.42.1.11", 8848))]
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=addresses):
            with self.assertRaisesRegex(ValueError, "DNS addresses changed"):
                probe_denied(self.config, {target: ["10.42.1.10"]})

    def test_allowed_source_rejects_dns_change_after_live_probe(self):
        resolved = {self.config["tlsHTTP"]: ["10.42.1.10"],
                    self.config["tlsGRPC"]: ["10.42.1.10"]}
        stable = [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.42.1.10", 8848))]
        changed = [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.42.1.11", 8848))]
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=stable):
            check_probe_dns(self.config, resolved)
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=changed):
            with self.assertRaisesRegex(ValueError, "DNS addresses changed during allowed probe"):
                check_probe_dns(self.config, resolved)

    def test_denied_source_fails_on_unknown_host(self):
        with patch("check_nacos_native_topology.socket.getaddrinfo", side_effect=socket.gaierror("unknown")):
            with self.assertRaises(socket.gaierror):
                probe_denied(self.config)

    def test_denied_source_does_not_count_missing_route_as_policy(self):
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=[
                (socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.42.1.10", 8848))]):
            with patch("check_nacos_native_topology.socket.create_connection",
                       side_effect=OSError(errno.ENETUNREACH, "Network is unreachable")):
                with self.assertRaisesRegex(ValueError, "inconclusive denied-source connection"):
                    probe_denied(self.config)

    def test_denied_source_fails_on_loopback_resolution(self):
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=[(None, None, None, None, ("127.0.0.1", 8848))]):
            with self.assertRaisesRegex(ValueError, "non-routable unicast"):
                probe_denied(self.config)

    def test_live_probes_reject_non_routable_dns_answers(self):
        for address in ("0.0.0.0", "224.0.0.1", "169.254.1.1", "::ffff:127.0.0.1"):
            with self.subTest(address=address):
                family = socket.AF_INET6 if ":" in address else socket.AF_INET
                answers = [(family, socket.SOCK_DGRAM, 0, "", (address, 8848))]
                with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=answers):
                    with self.assertRaisesRegex(ValueError, "non-routable unicast"):
                        source_ips(self.config, "allowed")
                with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=answers):
                    with self.assertRaisesRegex(ValueError, "non-routable unicast"):
                        probe_denied(self.config)

    def test_evidence_rejects_non_routable_dns_answer(self):
        targets = evidence(self.config, "allowed")["targets"]
        resolved = {target: ["10.42.1.10"] for target in targets}
        allowed = evidence(self.config, "allowed", {target: "10.42.0.7" for target in targets}, resolved,
                           tenant_namespaces=("namespace-a", "namespace-b"))
        allowed["validLoginCount"] = 2
        allowed["tlsCertSHA256"] = self.certificates(self.config, "10.42.1.10")
        denied_targets = evidence(self.config, "denied")["targets"]
        denied = evidence(self.config, "denied", {target: "10.43.0.7" for target in denied_targets},
                          {target: ["10.42.1.10"] for target in denied_targets})
        allowed["probeHost"] = "sdk-host"
        denied["probeHost"] = "blocked-host"
        allowed["resolvedIPs"][targets[0]] = ["0.0.0.0"]
        allowed["tlsCertSHA256"][targets[0]] = {"0.0.0.0": "a" * 64}
        with self.assertRaisesRegex(ValueError, "non-routable unicast"):
            verify_pair(self.config, allowed, denied)

    def test_denied_source_requires_external_backend_ip(self):
        for host in (None, "127.0.0.1", "invalid-host"):
            with self.subTest(host=host):
                config = copy.deepcopy(self.config)
                config["backendExternalHost"] = host
                with self.assertRaises(ValueError):
                    probe_denied(config)

    def test_source_classification_uses_routed_address(self):
        class RoutedSocket:
            def __init__(self, source):
                self.source = source

            def __enter__(self):
                return self

            def __exit__(self, *_):
                return False

            def connect(self, _):
                pass

            def getsockname(self):
                return self.source, 12345

        addresses = [(socket.AF_INET, socket.SOCK_DGRAM, 0, "", ("10.42.1.10", 8848))]
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=addresses):
            with patch("check_nacos_native_topology.socket.socket", return_value=RoutedSocket("10.42.0.7")):
                resolved = {}
                self.assertEqual(len(source_ips(self.config, "allowed", resolved)), 4)
                self.assertEqual(resolved["10.42.1.10:18848"], ["10.42.1.10"])
                self.assertEqual(resolved["10.42.1.10:19848"], ["10.42.1.10"])
                with self.assertRaisesRegex(ValueError, "wrong allowedSources classification"):
                    source_ips(self.config, "denied")
            with patch("check_nacos_native_topology.socket.socket", return_value=RoutedSocket("10.43.0.7")):
                self.assertEqual(len(source_ips(self.config, "denied")), 4)
                with self.assertRaisesRegex(ValueError, "wrong allowedSources classification"):
                    source_ips(self.config, "allowed")

    def test_pair_requires_live_distinct_classified_sources(self):
        allowed_targets = evidence(self.config, "allowed")["targets"]
        denied_targets = evidence(self.config, "denied")["targets"]
        resolved = {target: ["10.42.1.10"] for target in denied_targets}
        allowed = evidence(self.config, "allowed", {target: "10.42.0.7" for target in allowed_targets},
                           {target: resolved[target] for target in allowed_targets}, valid_login_count=2,
                           certificates=self.certificates(self.config, "10.42.1.10"),
                           tenant_namespaces=("namespace-a", "namespace-b"))
        denied = evidence(self.config, "denied", {target: "10.43.0.7" for target in denied_targets}, resolved)
        allowed["probeHost"] = "sdk-host"
        denied["probeHost"] = "blocked-host"
        verify_pair(self.config, allowed, denied)
        missing_host = copy.deepcopy(allowed)
        missing_host["probeHost"] = ""
        with self.assertRaisesRegex(ValueError, "lacks a probe host name"):
            verify_pair(self.config, missing_host, denied)
        old_allowed = copy.deepcopy(allowed)
        del old_allowed["backendDirectDenied"]
        with self.assertRaisesRegex(ValueError, "lacks plaintext backend denial check"):
            verify_pair(self.config, old_allowed, denied)
        old_allowed = copy.deepcopy(allowed)
        del old_allowed["authenticatedHTTPDenied"]
        with self.assertRaisesRegex(ValueError, "lacks authenticated HTTP config denial check"):
            verify_pair(self.config, old_allowed, denied)
        old_allowed = copy.deepcopy(allowed)
        del old_allowed["anonymousGRPCDenied"]
        with self.assertRaisesRegex(ValueError, "lacks anonymous gRPC read denial check"):
            verify_pair(self.config, old_allowed, denied)
        old_allowed = copy.deepcopy(allowed)
        del old_allowed["anonymousGRPCSubscriptionDenied"]
        with self.assertRaisesRegex(ValueError, "lacks anonymous gRPC subscription denial check"):
            verify_pair(self.config, old_allowed, denied)
        old_allowed = copy.deepcopy(allowed)
        del old_allowed["tenantGRPCIsolation"]
        with self.assertRaisesRegex(ValueError, "lacks two-tenant gRPC isolation check"):
            verify_pair(self.config, old_allowed, denied)
        old_allowed = copy.deepcopy(allowed)
        del old_allowed["tenantNamespaces"]
        with self.assertRaisesRegex(ValueError, "lacks two distinct tenant namespace IDs"):
            verify_pair(self.config, old_allowed, denied)
        old_allowed = copy.deepcopy(allowed)
        old_allowed["validLoginCount"] = 0
        with self.assertRaisesRegex(ValueError, "lacks two valid SDK login checks"):
            verify_pair(self.config, old_allowed, denied)
        old_allowed = copy.deepcopy(allowed)
        old_allowed["tlsCertSHA256"] = {}
        with self.assertRaisesRegex(ValueError, "lacks TLS certificate fingerprints"):
            verify_pair(self.config, old_allowed, denied)
        old_allowed = copy.deepcopy(allowed)
        del old_allowed["caSHA256"]
        with self.assertRaisesRegex(ValueError, "lacks a CA file SHA-256"):
            verify_pair(self.config, old_allowed, denied)
        rotated_ca = copy.deepcopy(denied)
        rotated_ca["caSHA256"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "different CA file contents"):
            verify_pair(self.config, allowed, rotated_ca)
        with open(self.config["caFile"], "wb") as stream:
            stream.write(b"rotated CA bundle")
        with self.assertRaisesRegex(ValueError, "local CA file differs"):
            verify_pair(self.config, allowed, denied)
        with open(self.config["caFile"], "wb") as stream:
            stream.write(b"test CA bundle")
        mapped = copy.deepcopy(allowed)
        mapped["sourceIPs"] = {target: "::ffff:10.42.0.7" for target in allowed_targets}
        mapped["resolvedIPs"] = {target: ["::ffff:10.42.1.10"] for target in allowed_targets}
        mapped["tlsCertSHA256"] = self.certificates(self.config, "::ffff:10.42.1.10")
        verify_pair(self.config, mapped, denied)
        changed_backend = copy.deepcopy(allowed)
        changed_backend["resolvedIPs"]["10.42.1.10:18848"] = ["10.42.1.11"]
        with self.assertRaisesRegex(ValueError, "literal target.*another IP"):
            verify_pair(self.config, changed_backend, denied)
        both_changed_backend = copy.deepcopy(denied)
        both_changed_backend["resolvedIPs"]["10.42.1.10:18848"] = ["10.42.1.11"]
        with self.assertRaisesRegex(ValueError, "literal target.*another IP"):
            verify_pair(self.config, changed_backend, both_changed_backend)
        for field, value, message in (
            ("probeHost", "sdk-host", "distinct hosts"),
            ("sourceIPs", {}, "lacks routed source"),
            ("resolvedIPs", {}, "lacks resolved IPs"),
            ("targets", [], "every configured target"),
            ("configSHA256", "wrong", "different deployment"),
            ("utc", (datetime.datetime.now(datetime.timezone.utc)
                     - datetime.timedelta(minutes=6)).isoformat(), "five minutes"),
        ):
            with self.subTest(field=field):
                changed = copy.deepcopy(denied)
                changed[field] = value
                with self.assertRaisesRegex(ValueError, message):
                    verify_pair(self.config, allowed, changed)
        changed = copy.deepcopy(denied)
        changed["resolvedIPs"][self.config["tlsHTTP"]] = ["10.42.1.12"]
        with self.assertRaisesRegex(ValueError, "resolved different IPs"):
            verify_pair(self.config, allowed, changed)
        # Offline verification can run on a third host without the probe CA file.
        os.unlink(self.config["caFile"])
        verify_pair(self.config, allowed, denied)

    def test_pair_rejects_stale_and_future_records_even_when_probes_match(self):
        allowed_targets = evidence(self.config, "allowed")["targets"]
        denied_targets = evidence(self.config, "denied")["targets"]
        allowed = evidence(self.config, "allowed", {target: "10.42.0.7" for target in allowed_targets},
                           {target: ["10.42.1.10"] for target in allowed_targets}, valid_login_count=2,
                           certificates=self.certificates(self.config, "10.42.1.10"),
                           tenant_namespaces=("namespace-a", "namespace-b"))
        denied = evidence(self.config, "denied", {target: "10.43.0.7" for target in denied_targets},
                          {target: ["10.42.1.10"] for target in denied_targets})
        allowed["probeHost"] = "sdk-host"
        denied["probeHost"] = "blocked-host"
        for age, message in ((datetime.timedelta(minutes=11), "older than ten minutes"),
                             (-datetime.timedelta(minutes=2), "future")):
            with self.subTest(age=age):
                timestamp = (datetime.datetime.now(datetime.timezone.utc) - age).isoformat()
                allowed["utc"] = timestamp
                denied["utc"] = timestamp
                with self.assertRaisesRegex(ValueError, message):
                    verify_pair(self.config, allowed, denied)

    def test_cluster_requires_two_distinct_nodes_in_same_window(self):
        other_config = copy.deepcopy(self.config)
        other_config.update(tlsHTTP="nacos-b.example:8848", tlsGRPC="nacos-b.example:9848",
                            backendExternalHost="10.42.1.11")

        def pair(config):
            addresses = {target: [config["backendExternalHost"]]
                         for target in evidence(config, "denied")["targets"]}
            allowed = evidence(config, "allowed", {
                target: "10.42.0.7" for target in evidence(config, "allowed")["targets"]},
                {target: addresses[target] for target in evidence(config, "allowed")["targets"]},
                valid_login_count=2,
                certificates=self.certificates(config, config["backendExternalHost"]),
                tenant_namespaces=("namespace-a", "namespace-b"))
            denied = evidence(config, "denied", {
                target: "10.43.0.7" for target in evidence(config, "denied")["targets"]}, addresses)
            allowed["probeHost"] = "sdk-host"
            denied["probeHost"] = "blocked-host"
            return allowed, denied

        records = [pair(self.config), pair(other_config)]
        verify_cluster([self.config, other_config], records)
        for index, mode in ((0, "allowed"), (1, "denied")):
            different_host = copy.deepcopy(records)
            different_host[1][index]["probeHost"] = f"other-{mode}-host"
            with self.assertRaisesRegex(ValueError, f"same {mode} host"):
                verify_cluster([self.config, other_config], different_host)
        different_tenants = copy.deepcopy(records)
        different_tenants[1][0]["tenantNamespaces"] = ["namespace-c", "namespace-d"]
        with self.assertRaisesRegex(ValueError, "different tenant namespaces"):
            verify_cluster([self.config, other_config], different_tenants)
        with self.assertRaisesRegex(ValueError, "distinct network endpoints"):
            verify_cluster([self.config, self.config], [records[0], records[0]])
        stale = copy.deepcopy(records)
        for record in stale[1]:
            record["utc"] = (datetime.datetime.now(datetime.timezone.utc)
                             - datetime.timedelta(minutes=6)).isoformat()
        with self.assertRaisesRegex(ValueError, "both nodes.*five minutes"):
            verify_cluster([self.config, other_config], stale)
        with self.assertRaisesRegex(ValueError, "different deployment"):
            verify_cluster([self.config, other_config], [records[0], records[0]])
        alias = copy.deepcopy(records)
        for record in alias[1]:
            record["resolvedIPs"][other_config["tlsHTTP"]] = ["10.42.1.10"]
        alias[1][0]["tlsCertSHA256"][other_config["tlsHTTP"]] = {"10.42.1.10": "a" * 64}
        with self.assertRaisesRegex(ValueError, "shared IP address"):
            verify_cluster([self.config, other_config], alias)
        mapped_backend = copy.deepcopy(other_config)
        mapped_backend["backendExternalHost"] = "::ffff:10.42.1.10"
        with self.assertRaisesRegex(ValueError, "share backendExternalHost"):
            verify_cluster([self.config, mapped_backend], [records[0], pair(mapped_backend)])
        mapped_tls = copy.deepcopy(records)
        for record in mapped_tls[1]:
            record["resolvedIPs"][other_config["tlsHTTP"]] = ["::ffff:10.42.1.10"]
        mapped_tls[1][0]["tlsCertSHA256"][other_config["tlsHTTP"]] = {"::ffff:10.42.1.10": "a" * 64}
        with self.assertRaisesRegex(ValueError, "shared IP address"):
            verify_cluster([self.config, other_config], mapped_tls)


if __name__ == "__main__":
    unittest.main()
