#!/usr/bin/env python3
"""Fail closed on unsupported native Nacos deployment declarations; optionally probe TLS."""
import argparse
import datetime
import errno
import hashlib
import ipaddress
import json
import os
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


def source_ips(c, mode):
    """Record the local routed address; NAT may change it after this host."""
    targets = [split_address(c[field]) for field in ("tlsHTTP", "tlsGRPC")]
    if mode == "denied":
        targets += [(c["backendExternalHost"], split_address(c[field])[1])
                    for field in ("backendHTTP", "backendGRPC")]
    networks = [ipaddress.ip_network(value, strict=False) for value in c["allowedSources"]]
    result = {}
    for host, port in targets:
        addresses = socket.getaddrinfo(host, port, type=socket.SOCK_DGRAM)
        if not addresses or any(ipaddress.ip_address(addr[4][0]).is_loopback for addr in addresses):
            raise ValueError(f"target {host} resolves to loopback or no address")
        routed = set()
        for family, kind, proto, _, address in addresses:
            with socket.socket(family, kind, proto) as sock:
                sock.connect(address)
                source = ipaddress.ip_address(sock.getsockname()[0])
            permitted = any(source in network for network in networks)
            if permitted != (mode == "allowed"):
                raise ValueError(f"{mode} source {source} has wrong allowedSources classification for {host}:{port}")
            routed.add(str(source))
        if len(routed) != 1:
            raise ValueError(f"target {host}:{port} uses different routed source IPs: {sorted(routed)}")
        result[net_address(host, port)] = routed.pop()
    return result


def net_address(host, port):
    return f"[{host}]:{port}" if ":" in host else f"{host}:{port}"


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
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context))
    for request, expected in (
        (urllib.request.Request(base + "/nacos/v1/cs/configs?tenant=x&group=GOLAB_FLAGS&dataId=golab.manifest"), 404),
        (urllib.request.Request(base + "/nacos/v1/auth/users/login",
                                data=urllib.parse.urlencode({"username": "golab-sdk", "password": "invalid"}).encode()), 401),
    ):
        try:
            with opener.open(request, timeout=5) as response:
                status = response.status
        except urllib.error.HTTPError as exc:
            status = exc.code
        if status != expected:
            raise ValueError(f"{request.full_url}: HTTP {status}, expected {expected}")


def probe_denied(c):
    """Run from a source outside allowedSources while an allowed probe passes."""
    host = c.get("backendExternalHost")
    if not host or ipaddress.ip_address(host).is_loopback:
        raise ValueError("backendExternalHost must be a non-loopback IP visible from the denied source")
    targets = [split_address(c[field]) for field in ("tlsHTTP", "tlsGRPC")]
    targets += [(host, split_address(c[field])[1]) for field in ("backendHTTP", "backendGRPC")]
    for target in targets:
        # A misspelled/unresolvable hostname is not evidence of network policy.
        addresses = socket.getaddrinfo(target[0], target[1], type=socket.SOCK_STREAM)
        if not addresses or any(ipaddress.ip_address(address[4][0]).is_loopback for address in addresses):
            raise ValueError(f"target {target[0]} resolves to loopback or no address")
        for address in addresses:
            endpoint = (address[4][0], target[1])
            try:
                with socket.create_connection(endpoint, timeout=3):
                    raise ValueError(f"unauthorized source connected to {endpoint[0]}:{endpoint[1]}")
            except OSError as exc:
                if isinstance(exc, (socket.timeout, ConnectionRefusedError)) or exc.errno == errno.ECONNREFUSED:
                    continue
                raise ValueError(f"inconclusive denied-source connection to {endpoint[0]}:{endpoint[1]}: {exc}") from exc


def evidence(c, mode, sources=None):
    return {
        "mode": mode,
        "result": "PASS",
        "utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "probeHost": socket.gethostname(),
        "sourceIPs": sources or {},
        "configSHA256": hashlib.sha256(json.dumps(c, sort_keys=True).encode()).hexdigest(),
        "targets": [c["tlsHTTP"], c["tlsGRPC"]] + (
            [f"{c['backendExternalHost']}:{split_address(c[field])[1]}" for field in ("backendHTTP", "backendGRPC")]
            if mode == "denied" else []
        ),
    }


def verify_pair(c, allowed, denied):
    expected = hashlib.sha256(json.dumps(c, sort_keys=True).encode()).hexdigest()
    for record, mode in ((allowed, "allowed"), (denied, "denied")):
        if record.get("mode") != mode or record.get("result") != "PASS":
            raise ValueError(f"missing successful {mode} live probe")
        if record.get("configSHA256") != expected:
            raise ValueError(f"{mode} evidence uses a different deployment declaration")
        if record.get("targets") != evidence(c, mode)["targets"]:
            raise ValueError(f"{mode} evidence does not cover every configured target")
        if not isinstance(record.get("sourceIPs"), dict) or not record["sourceIPs"]:
            raise ValueError(f"{mode} evidence lacks routed source IPs")
        if set(record["sourceIPs"]) != set(record["targets"]):
            raise ValueError(f"{mode} evidence source IPs do not cover every target")
    times = [datetime.datetime.fromisoformat(record["utc"]) for record in (allowed, denied)]
    if any(value.tzinfo is None for value in times) or abs((times[0] - times[1]).total_seconds()) > 300:
        raise ValueError("live probes must be within five minutes")
    if allowed["probeHost"] == denied["probeHost"]:
        raise ValueError("allowed and denied probes must run on distinct hosts")
    networks = [ipaddress.ip_network(value, strict=False) for value in c["allowedSources"]]
    for record, permitted in ((allowed, True), (denied, False)):
        for source in record["sourceIPs"].values():
            if any(ipaddress.ip_address(source) in network for network in networks) != permitted:
                raise ValueError(f"{record['mode']} source {source} has wrong allowedSources classification")


def verify_cluster(configs, records):
    if len(configs) != 2 or len(records) != 2:
        raise ValueError("exactly two nodes and their allowed/denied evidence are required")
    if any(validate(config) for config in configs):
        raise ValueError("both node declarations must pass topology validation")
    for config, (allowed, denied) in zip(configs, records):
        verify_pair(config, allowed, denied)
    for field in ("tlsHTTP", "tlsGRPC", "backendExternalHost"):
        if configs[0].get(field) == configs[1].get(field):
            raise ValueError(f"two nodes share {field}; distinct network endpoints are required")
    times = [datetime.datetime.fromisoformat(record["utc"])
             for pair in records for record in pair]
    if max(times) - min(times) > datetime.timedelta(minutes=5):
        raise ValueError("both nodes' live probes must be within five minutes")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("config", help="deployment declaration JSON")
    parser.add_argument("--probe", action="store_true", help="also check live TLS and HTTP deny paths")
    parser.add_argument("--probe-denied", action="store_true", help="from a rejected source, verify both TLS and backend ports refuse TCP")
    parser.add_argument("--evidence", help="write successful probe metadata as JSON; no credentials are recorded")
    parser.add_argument("--verify-pair", nargs=2, metavar=("ALLOWED", "DENIED"),
                        help="require same-config, distinct-source live probes within five minutes")
    parser.add_argument("--verify-cluster", nargs=5,
                        metavar=("NODE_B_CONFIG", "A_ALLOWED", "A_DENIED", "B_ALLOWED", "B_DENIED"),
                        help="require paired live evidence for two distinct nodes within five minutes")
    args = parser.parse_args()
    if args.probe and args.probe_denied:
        parser.error("--probe and --probe-denied are mutually exclusive")
    if args.verify_pair and (args.probe or args.probe_denied or args.evidence):
        parser.error("--verify-pair cannot be combined with live probes or --evidence")
    if args.verify_cluster and (args.verify_pair or args.probe or args.probe_denied or args.evidence):
        parser.error("--verify-cluster cannot be combined with other checks")
    if args.evidence and os.path.lexists(args.evidence):
        print("ALERT: evidence path already exists; use a new filename for each run", file=sys.stderr)
        return 1
    with open(args.config, encoding="utf-8") as stream:
        config = json.load(stream)
    errors = validate(config)
    if errors:
        for error in errors:
            print("ALERT:", error, file=sys.stderr)
        return 1
    if args.verify_cluster:
        try:
            paths = args.verify_cluster
            with open(paths[0], encoding="utf-8") as stream:
                other_config = json.load(stream)
            records = []
            for allowed_path, denied_path in ((paths[1], paths[2]), (paths[3], paths[4])):
                with open(allowed_path, encoding="utf-8") as stream:
                    allowed = json.load(stream)
                with open(denied_path, encoding="utf-8") as stream:
                    denied = json.load(stream)
                records.append((allowed, denied))
            verify_cluster((config, other_config), records)
        except (OSError, ValueError, KeyError, TypeError) as exc:
            print("ALERT: two-node evidence failed:", exc, file=sys.stderr)
            return 1
        print("PASS: paired live probes for two distinct TLS/L4 nodes")
        return 0
    if args.verify_pair:
        try:
            with open(args.verify_pair[0], encoding="utf-8") as stream:
                allowed = json.load(stream)
            with open(args.verify_pair[1], encoding="utf-8") as stream:
                denied = json.load(stream)
            verify_pair(config, allowed, denied)
        except (OSError, ValueError, KeyError, TypeError) as exc:
            print("ALERT: evidence pair failed:", exc, file=sys.stderr)
            return 1
        print("PASS: paired live probes from distinct classified sources")
        return 0
    sources = None
    if args.probe:
        try:
            sources = source_ips(config, "allowed")
            probe(config)
        except (OSError, ValueError, urllib.error.URLError) as exc:
            print("ALERT: live probe failed:", exc, file=sys.stderr)
            return 1
    if args.probe_denied:
        try:
            sources = source_ips(config, "denied")
            probe_denied(config)
        except (OSError, ValueError) as exc:
            print("ALERT: denied-source probe failed:", exc, file=sys.stderr)
            return 1
    mode = "denied" if args.probe_denied else "allowed" if args.probe else "declaration"
    if args.evidence:
        output = evidence(config, mode, sources)
        temporary = f"{args.evidence}.tmp.{os.getpid()}"
        try:
            with open(temporary, "w", encoding="utf-8") as stream:
                json.dump(output, stream, indent=2)
                stream.write("\n")
            os.replace(temporary, args.evidence)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
    print("PASS: declared TLS/L4 topology" + (" and live TLS/HTTP probes" if args.probe else " and denied-source TCP probes" if args.probe_denied else ""))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
