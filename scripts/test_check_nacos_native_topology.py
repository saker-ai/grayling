import copy
import socket
import unittest
from unittest.mock import patch

from check_nacos_native_topology import evidence, probe_denied, validate


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
            ("nacos.example", 8848), ("nacos.example", 9848),
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


if __name__ == "__main__":
    unittest.main()
