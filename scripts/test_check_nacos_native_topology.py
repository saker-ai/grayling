import copy
import unittest

from check_nacos_native_topology import validate


class NativeTopologyTest(unittest.TestCase):
    def setUp(self):
        self.config = {
            "mode": "tls-l4-per-client-tcp", "nativeAuth": True,
            "backendConnectionReuse": False, "backendHTTP": "127.0.0.1:18848",
            "backendGRPC": "127.0.0.1:19848", "tlsHTTP": "nacos.example:8848",
            "tlsGRPC": "nacos.example:9848", "caFile": "/tmp/ca.pem",
            "allowedSources": ["10.42.0.0/16"],
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


if __name__ == "__main__":
    unittest.main()
