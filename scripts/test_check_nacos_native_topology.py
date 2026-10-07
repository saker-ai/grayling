import copy
import datetime
import socket
import unittest
from unittest.mock import patch

from check_nacos_native_topology import evidence, probe_denied, source_ips, validate, verify_pair


class NativeTopologyTest(unittest.TestCase):
    def setUp(self):
        self.config = {
            "mode": "tls-l4-per-client-tcp", "nativeAuth": True,
            "backendConnectionReuse": False, "backendHTTP": "127.0.0.1:18848",
            "backendGRPC": "127.0.0.1:19848", "tlsHTTP": "nacos.example:8848",
            "tlsGRPC": "nacos.example:9848", "caFile": "/tmp/ca.pem",
            "allowedSources": ["10.42.0.0/16"],
            "backendExternalHost": "10.42.1.10",
        }

    def test_supported_declaration(self):
        self.assertEqual(validate(self.config), [])

    def test_unsafe_topologies_fail_closed(self):
        for field, value in (
            ("mode", "l7-http2"), ("nativeAuth", False),
            ("backendConnectionReuse", True), ("backendGRPC", "0.0.0.0:19848"),
            ("allowedSources", ["0.0.0.0/0"]), ("caFile", ""),
        ):
            with self.subTest(field=field):
                config = copy.deepcopy(self.config)
                config[field] = value
                self.assertTrue(validate(config))

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

    def test_denied_source_fails_on_unknown_host(self):
        with patch("check_nacos_native_topology.socket.getaddrinfo", side_effect=socket.gaierror("unknown")):
            with self.assertRaises(socket.gaierror):
                probe_denied(self.config)

    def test_denied_source_fails_on_loopback_resolution(self):
        with patch("check_nacos_native_topology.socket.getaddrinfo", return_value=[(None, None, None, None, ("127.0.0.1", 8848))]):
            with self.assertRaisesRegex(ValueError, "resolves to loopback"):
                probe_denied(self.config)

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
                self.assertEqual(len(source_ips(self.config, "allowed")), 2)
                with self.assertRaisesRegex(ValueError, "wrong allowedSources classification"):
                    source_ips(self.config, "denied")
            with patch("check_nacos_native_topology.socket.socket", return_value=RoutedSocket("10.43.0.7")):
                self.assertEqual(len(source_ips(self.config, "denied")), 4)
                with self.assertRaisesRegex(ValueError, "wrong allowedSources classification"):
                    source_ips(self.config, "allowed")

    def test_pair_requires_live_distinct_classified_sources(self):
        allowed_targets = evidence(self.config, "allowed")["targets"]
        denied_targets = evidence(self.config, "denied")["targets"]
        allowed = evidence(self.config, "allowed", {target: "10.42.0.7" for target in allowed_targets})
        denied = evidence(self.config, "denied", {target: "10.43.0.7" for target in denied_targets})
        allowed["probeHost"] = "sdk-host"
        denied["probeHost"] = "blocked-host"
        verify_pair(self.config, allowed, denied)
        for field, value, message in (
            ("probeHost", "sdk-host", "distinct hosts"),
            ("sourceIPs", {}, "lacks routed source"),
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


if __name__ == "__main__":
    unittest.main()
