#!/usr/bin/env python3
"""Fail closed on unsupported native Nacos deployment declarations; optionally probe TLS."""
import argparse
import ipaddress
import json
import socket
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request


def validate(c):
    errors = []
    if c.get("mode") != "tls-l4-per-client-tcp":
        errors.append("unsupported mode: L7 HTTP/2 backend connection reuse is not supported")
    if c.get("nativeAuth") is not True:
        errors.append("-nacos-native-auth=true is required")
    if c.get("backendConnectionReuse") is not False:
        errors.append("one inbound client TCP connection must map to one backend TCP connection")
    for field in ("backendHTTP", "backendGRPC"):
        try:
            host, port = split_address(c[field])
            if not ipaddress.ip_address(host).is_loopback or not 0 < port < 65536:
                raise ValueError("backend must bind loopback")
        except (KeyError, ValueError) as exc:
            errors.append(f"{field}: {exc}")
    for field in ("tlsHTTP", "tlsGRPC"):
        try:
            host, port = split_address(c[field])
            if not host or not 0 < port < 65536:
                raise ValueError("invalid TLS endpoint")
        except (KeyError, ValueError) as exc:
            errors.append(f"{field}: {exc}")
    if c.get("tlsHTTP") == c.get("tlsGRPC"):
        errors.append("HTTP login and gRPC need distinct TLS ports")
    if not c.get("caFile"):
        errors.append("caFile is required for verified TLS")
    sources = c.get("allowedSources")
    if not isinstance(sources, list) or not sources:
        errors.append("allowedSources must be a nonempty explicit list")
    else:
        for source in sources:
            try:
                network = ipaddress.ip_network(source, strict=False)
                if network.prefixlen == 0:
                    raise ValueError("unrestricted source")
            except ValueError as exc:
                errors.append(f"allowedSources {source}: {exc}")
    return errors


def split_address(value):
    if value.startswith("["):
        host, sep, port = value[1:].partition("]:")
    else:
        host, sep, port = value.rpartition(":")
    if not sep:
        raise ValueError("expected host:port")
    return host, int(port)


def probe(c):
    context = ssl.create_default_context(cafile=c["caFile"])
    host, port = split_address(c["tlsGRPC"])
    context.set_alpn_protocols(["h2"])
    with socket.create_connection((host, port), timeout=5) as sock:
        with context.wrap_socket(sock, server_hostname=host) as tls_sock:
            if tls_sock.selected_alpn_protocol() != "h2":
                raise ValueError("gRPC TLS listener did not negotiate h2")
    context.set_alpn_protocols(["http/1.1"])
    host, port = split_address(c["tlsHTTP"])
    base = f"https://{host}:{port}"
    for request, expected in (
        (urllib.request.Request(base + "/nacos/v1/cs/configs?tenant=x&group=GOLAB_FLAGS&dataId=golab.manifest"), 404),
        (urllib.request.Request(base + "/nacos/v1/auth/users/login",
                                data=urllib.parse.urlencode({"username": "golab-sdk", "password": "invalid"}).encode()), 401),
    ):
        try:
            with urllib.request.urlopen(request, context=context, timeout=5) as response:
                status = response.status
        except urllib.error.HTTPError as exc:
            status = exc.code
        if status != expected:
            raise ValueError(f"{request.full_url}: HTTP {status}, expected {expected}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("config", help="deployment declaration JSON")
    parser.add_argument("--probe", action="store_true", help="also check live TLS and HTTP deny paths")
    args = parser.parse_args()
    with open(args.config, encoding="utf-8") as stream:
        config = json.load(stream)
    errors = validate(config)
    if errors:
        for error in errors:
            print("ALERT:", error, file=sys.stderr)
        return 1
    if args.probe:
        try:
            probe(config)
        except (OSError, ValueError, urllib.error.URLError) as exc:
            print("ALERT: live probe failed:", exc, file=sys.stderr)
            return 1
    print("PASS: declared TLS/L4 topology" + (" and live TLS/deny probes" if args.probe else ""))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
