#!/usr/bin/env python3
"""Fail closed on unsupported native Nacos deployment declarations; optionally probe TLS."""
import argparse
import datetime
import errno
import hashlib
import http.client
import ipaddress
import json
import os
import socket
import ssl
import subprocess
import sys
import tempfile
import urllib.parse


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
    try:
        if split_address(c["backendHTTP"])[1] == split_address(c["backendGRPC"])[1]:
            errors.append("HTTP login and gRPC need distinct backend ports")
    except (KeyError, ValueError):
        pass  # The field-specific checks above report malformed addresses.
    try:
        external_ip = ipaddress.ip_address(c["backendExternalHost"])
        if not routable_unicast(str(external_ip)):
            raise ValueError("must be a routable unicast IP reachable from the denied source")
    except (KeyError, ValueError) as exc:
        errors.append(f"backendExternalHost: {exc}")
    for field in ("tlsHTTP", "tlsGRPC"):
        try:
            host, port = split_address(c[field])
            if not host or not 0 < port < 65536:
                raise ValueError("invalid TLS endpoint")
            normalized_host = host.lower().rstrip(".")
            if (host == "*" or normalized_host in ("localhost", "ip6-localhost")
                    or normalized_host.endswith(".localhost")):
                raise ValueError("TLS endpoint must be reachable from a separate client host")
            try:
                address = ipaddress.ip_address(host)
            except ValueError:
                pass  # A DNS name is checked again by the live probe.
            else:
                if not routable_unicast(str(address)):
                    raise ValueError("TLS endpoint must be a routable unicast address")
        except (KeyError, ValueError) as exc:
            errors.append(f"{field}: {exc}")
    try:
        if split_address(c["tlsHTTP"])[1] == split_address(c["tlsGRPC"])[1]:
            errors.append("HTTP login and gRPC need distinct TLS ports")
    except (KeyError, ValueError):
        pass  # The field-specific checks above report malformed addresses.
    if not c.get("caFile"):
        errors.append("caFile is required for verified TLS")
    sources = c.get("allowedSources")
    if not isinstance(sources, list) or not sources:
        errors.append("allowedSources must be a nonempty explicit list")
    else:
        networks_by_version = {4: [], 6: []}
        invalid_source_ranges = {
            4: [ipaddress.ip_network(value) for value in (
                "0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4")],
            6: [ipaddress.ip_network(value) for value in (
                "::/128", "::1/128", "fe80::/10", "ff00::/8", "::ffff:0:0/96")],
        }
        for source in sources:
            try:
                network = ipaddress.ip_network(source, strict=False)
                if network.prefixlen == 0:
                    raise ValueError("unrestricted source")
                networks_by_version[network.version].append(network)
                if any(network.overlaps(excluded) for excluded in invalid_source_ranges[network.version]):
                    errors.append(f"allowedSources {source}: source range includes unspecified, loopback, link-local, multicast, or IPv4-mapped addresses")
            except ValueError as exc:
                errors.append(f"allowedSources {source}: {exc}")
        for version, networks in networks_by_version.items():
            if any(network.prefixlen == 0 for network in ipaddress.collapse_addresses(networks)):
                errors.append(f"allowedSources collectively permit the entire IPv{version} address space")
    return errors


def split_address(value):
    if value.startswith("["):
        host, sep, port = value[1:].partition("]:")
    else:
        host, sep, port = value.rpartition(":")
    if not sep:
        raise ValueError("expected host:port")
    return host, int(port)


def routable_unicast(value):
    address = canonical_ip(value)
    return not (address.is_loopback or address.is_unspecified
                or address.is_multicast or address.is_link_local)


def canonical_ip(value):
    """Treat an IPv4 address and its IPv4-mapped IPv6 form as one endpoint."""
    address = ipaddress.ip_address(value)
    if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped:
        return address.ipv4_mapped
    return address


def source_ips(c, mode, resolved=None):
    """Record the local routed address; NAT may change it after this host."""
    targets = [split_address(c[field]) for field in ("tlsHTTP", "tlsGRPC")]
    targets += backend_targets(c)
    networks = [ipaddress.ip_network(value, strict=False) for value in c["allowedSources"]]
    result = {}
    for host, port in targets:
        addresses = socket.getaddrinfo(host, port, type=socket.SOCK_DGRAM)
        if not addresses or any(not routable_unicast(addr[4][0]) for addr in addresses):
            raise ValueError(f"target {host} resolves to a non-routable unicast address or no address")
        if resolved is not None:
            resolved[net_address(host, port)] = sorted({addr[4][0] for addr in addresses})
        routed = set()
        for family, kind, proto, _, address in addresses:
            with socket.socket(family, kind, proto) as sock:
                sock.connect(address)
                source = canonical_ip(sock.getsockname()[0])
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


def check_probe_dns(c, resolved):
    """Reject a changed endpoint between route selection and live TLS/HTTP probes."""
    for field in ("tlsHTTP", "tlsGRPC"):
        host, port = split_address(c[field])
        target = net_address(host, port)
        addresses = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
        actual = sorted({address[4][0] for address in addresses})
        if not actual or any(not routable_unicast(address) for address in actual) or actual != resolved[target]:
            raise ValueError(f"DNS addresses changed during allowed probe for {target}")


class PinnedHTTPSConnection(http.client.HTTPSConnection):
    """Connect to a probed IP while retaining the configured TLS name and Host."""

    def __init__(self, host, port, address, context):
        super().__init__(host, port, timeout=5, context=context)
        self.address = address

    def connect(self):
        sock = socket.create_connection((self.address, self.port), timeout=self.timeout)
        try:
            self.sock = self._context.wrap_socket(sock, server_hostname=self.host)
            if self.sock.selected_alpn_protocol() != "http/1.1":
                raise ValueError(f"HTTP TLS listener {self.address}:{self.port} did not negotiate http/1.1")
        except BaseException:
            if self.sock is not None:
                self.sock.close()
            else:
                sock.close()
            raise


def probe(c, resolved, login_tokens, tenant_namespaces):
    if len(login_tokens) != 2 or len(tenant_namespaces) != 2:
        raise ValueError("two login tokens and matching tenant namespaces are required")
    context = ssl.create_default_context(cafile=c["caFile"])
    certificates = {}
    host, port = split_address(c["tlsGRPC"])
    grpc_target = net_address(host, port)
    certificates[grpc_target] = {}
    context.set_alpn_protocols(["h2"])
    for address in resolved[grpc_target]:
        with socket.create_connection((address, port), timeout=5) as sock:
            with context.wrap_socket(sock, server_hostname=host) as tls_sock:
                if tls_sock.selected_alpn_protocol() != "h2":
                    raise ValueError(f"gRPC TLS listener {address} did not negotiate h2")
                certificates[grpc_target][address] = hashlib.sha256(
                    tls_sock.getpeercert(binary_form=True)).hexdigest()
    context.set_alpn_protocols(["http/1.1"])
    host, port = split_address(c["tlsHTTP"])
    http_target = net_address(host, port)
    certificates[http_target] = {}
    config_path = "/nacos/v1/cs/configs?tenant=x&group=GRAYLING_FLAGS&dataId=grayling.manifest"
    requests = [
        ("GET", config_path, None, 404, None, {}),
        ("POST", "/nacos/v1/auth/users/login",
         urllib.parse.urlencode({"username": "golab-sdk", "password": "invalid"}), 401, None, {}),
    ]
    for namespace in tenant_namespaces:
        requests.append(("GET", "/nacos/v1/cs/configs?" + urllib.parse.urlencode({
            "tenant": namespace, "group": "GRAYLING_FLAGS", "dataId": "grayling.manifest"}),
            None, 404, None, {}))
    requests = tuple(requests)
    for address in resolved[http_target]:
        authorized = tuple(request for token, namespace in zip(login_tokens, tenant_namespaces) for request in (
            ("POST", "/nacos/v1/auth/users/login",
             urllib.parse.urlencode({"username": "golab-sdk", "password": token}), 200, token, {}),
            ("GET", "/nacos/v1/cs/configs?" + urllib.parse.urlencode({
                "tenant": namespace, "group": "GRAYLING_FLAGS", "dataId": "grayling.manifest"}),
             None, 404, None, {"accessToken": token}),
        ))
        for method, path, body, expected, token, extra_headers in requests + authorized:
            connection = PinnedHTTPSConnection(host, port, address, context)
            try:
                headers = {"Content-Type": "application/x-www-form-urlencoded"} if body else {}
                headers.update(extra_headers)
                connection.request(method, path, body=body, headers=headers)
                fingerprint = hashlib.sha256(connection.sock.getpeercert(binary_form=True)).hexdigest()
                previous = certificates[http_target].setdefault(address, fingerprint)
                if previous != fingerprint:
                    raise ValueError(f"HTTP TLS certificate changed during probe for {address}:{port}")
                response = connection.getresponse()
                status = response.status
                login_body = response.read(16385) if token is not None and status == 200 else None
            finally:
                connection.close()
            if status != expected:
                raise ValueError(f"{address}:{port}{path}: HTTP {status}, expected {expected}")
            if token is not None:
                try:
                    result = json.loads(login_body) if len(login_body) <= 16384 else None
                except (TypeError, ValueError):
                    result = None
                if (not isinstance(result, dict) or result.get("accessToken") != token
                        or type(result.get("tokenTtl")) is not int or result["tokenTtl"] <= 0):
                    raise ValueError(f"{address}:{port}{path}: invalid SDK login response")
    return certificates


def probe_closed_targets(targets, resolved=None, label="unauthorized source"):
    """Require every address of each target to reject TCP connections."""
    for target in targets:
        # A misspelled/unresolvable hostname is not evidence of network policy.
        addresses = socket.getaddrinfo(target[0], target[1], type=socket.SOCK_STREAM)
        if not addresses or any(not routable_unicast(address[4][0]) for address in addresses):
            raise ValueError(f"target {target[0]} resolves to a non-routable unicast address or no address")
        if resolved is not None:
            name = net_address(*target)
            actual = sorted({address[4][0] for address in addresses})
            if resolved.get(name) != actual:
                raise ValueError(f"DNS addresses changed during TCP refusal probe for {name}")
        for address in addresses:
            endpoint = (address[4][0], target[1])
            try:
                with socket.create_connection(endpoint, timeout=3):
                    raise ValueError(f"{label} connected to {endpoint[0]}:{endpoint[1]}")
            except OSError as exc:
                if isinstance(exc, (socket.timeout, ConnectionRefusedError)) or exc.errno == errno.ECONNREFUSED:
                    continue
                raise ValueError(f"inconclusive denied-source connection to {endpoint[0]}:{endpoint[1]}: {exc}") from exc


def backend_targets(c):
    host = c.get("backendExternalHost")
    if not host or not routable_unicast(host):
        raise ValueError("backendExternalHost must be a routable unicast IP visible from both probe sources")
    return [(host, split_address(c[field])[1]) for field in ("backendHTTP", "backendGRPC")]


def probe_denied(c, resolved=None):
    """Run from a source outside allowedSources while an allowed probe passes."""
    targets = [split_address(c[field]) for field in ("tlsHTTP", "tlsGRPC")]
    probe_closed_targets(targets + backend_targets(c), resolved)


def evidence_targets(c):
    return [c["tlsHTTP"], c["tlsGRPC"]] + [
        net_address(*target) for target in backend_targets(c)
    ]


def evidence(c, mode, sources=None, resolved=None, valid_login_count=0, ca_digest=None,
             certificates=None, tenant_namespaces=()):
    return {
        "mode": mode,
        "result": "PASS",
        "backendDirectDenied": mode == "allowed",
        "anonymousTenantHTTPDenied": mode == "allowed",
        "authenticatedHTTPDenied": mode == "allowed",
        "anonymousGRPCDenied": mode == "allowed",
        "anonymousGRPCSubscriptionDenied": mode == "allowed",
        "tenantGRPCIsolation": mode == "allowed",
        "tenantNamespaces": list(tenant_namespaces),
        "validLoginCount": valid_login_count,
        "utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "probeHost": socket.gethostname(),
        "sourceIPs": sources or {},
        "resolvedIPs": resolved or {},
        "configSHA256": hashlib.sha256(json.dumps(c, sort_keys=True).encode()).hexdigest(),
        "caSHA256": (ca_digest or ca_sha256(c)) if mode in ("allowed", "denied") else None,
        "tlsCertSHA256": certificates or {},
        "targets": evidence_targets(c),
    }


def ca_sha256(c):
    with open(c["caFile"], "rb") as stream:
        return hashlib.sha256(stream.read()).hexdigest()


def write_evidence(path, output):
    """Publish a complete evidence file without replacing a concurrent writer."""
    fd, temporary = tempfile.mkstemp(prefix=f".{os.path.basename(path)}.",
                                     dir=os.path.dirname(path) or ".")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(output, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.link(temporary, path)
    finally:
        os.unlink(temporary)


def verify_pair(c, allowed, denied):
    expected = hashlib.sha256(json.dumps(c, sort_keys=True).encode()).hexdigest()
    for record, mode in ((allowed, "allowed"), (denied, "denied")):
        if record.get("mode") != mode or record.get("result") != "PASS":
            raise ValueError(f"missing successful {mode} live probe")
        if record.get("configSHA256") != expected:
            raise ValueError(f"{mode} evidence uses a different deployment declaration")
        digest = record.get("caSHA256")
        if not isinstance(digest, str) or len(digest) != 64 or any(ch not in "0123456789abcdef" for ch in digest):
            raise ValueError(f"{mode} evidence lacks a CA file SHA-256; rerun the live probe")
        if mode == "allowed" and record.get("backendDirectDenied") is not True:
            raise ValueError("allowed evidence lacks plaintext backend denial check; rerun the live probe")
        if mode == "allowed" and record.get("anonymousTenantHTTPDenied") is not True:
            raise ValueError("allowed evidence lacks anonymous tenant HTTP config denial check; rerun the live probe")
        if mode == "allowed" and record.get("authenticatedHTTPDenied") is not True:
            raise ValueError("allowed evidence lacks authenticated HTTP config denial check; rerun the live probe")
        if mode == "allowed" and record.get("anonymousGRPCDenied") is not True:
            raise ValueError("allowed evidence lacks anonymous gRPC read denial check; rerun the live probe")
        if mode == "allowed" and record.get("anonymousGRPCSubscriptionDenied") is not True:
            raise ValueError("allowed evidence lacks anonymous gRPC subscription denial check; rerun the live probe")
        if mode == "allowed" and record.get("tenantGRPCIsolation") is not True:
            raise ValueError("allowed evidence lacks two-tenant gRPC isolation check; rerun the live probe")
        namespaces = record.get("tenantNamespaces")
        if mode == "allowed" and (not isinstance(namespaces, list) or len(namespaces) != 2
                                  or any(not isinstance(value, str) or not value for value in namespaces)
                                  or namespaces[0] == namespaces[1]):
            raise ValueError("allowed evidence lacks two distinct tenant namespace IDs; rerun the live probe")
        if mode == "allowed" and record.get("validLoginCount") != 2:
            raise ValueError("allowed evidence lacks two valid SDK login checks; rerun the live probe")
        if mode == "allowed":
            certs = record.get("tlsCertSHA256")
            if not isinstance(certs, dict) or set(certs) != {c["tlsHTTP"], c["tlsGRPC"]}:
                raise ValueError("allowed evidence lacks TLS certificate fingerprints; rerun the live probe")
            for target in (c["tlsHTTP"], c["tlsGRPC"]):
                resolved = record.get("resolvedIPs", {})
                if not isinstance(certs[target], dict) or set(certs[target]) != set(resolved.get(target, [])):
                    raise ValueError(f"allowed evidence lacks TLS certificate fingerprints for {target}")
                for digest in certs[target].values():
                    if (not isinstance(digest, str) or len(digest) != 64
                            or any(ch not in "0123456789abcdef" for ch in digest)):
                        raise ValueError(f"allowed evidence has invalid TLS certificate fingerprint for {target}")
        if record.get("targets") != evidence_targets(c):
            raise ValueError(f"{mode} evidence does not cover every configured target")
        if not isinstance(record.get("sourceIPs"), dict) or not record["sourceIPs"]:
            raise ValueError(f"{mode} evidence lacks routed source IPs")
        if set(record["sourceIPs"]) != set(record["targets"]):
            raise ValueError(f"{mode} evidence source IPs do not cover every target")
        resolved = record.get("resolvedIPs")
        if not isinstance(resolved, dict) or set(resolved) != set(record["targets"]):
            raise ValueError(f"{mode} evidence lacks resolved IPs for every target")
        for target, addresses in resolved.items():
            if not isinstance(addresses, list) or not addresses:
                raise ValueError(f"{mode} evidence has no resolved IPs for {target}")
            if any(not routable_unicast(address) for address in addresses):
                raise ValueError(f"{mode} evidence resolves {target} to a non-routable unicast address")
            host, _ = split_address(target)
            try:
                literal = canonical_ip(host)
            except ValueError:
                continue
            if {canonical_ip(address) for address in addresses} != {literal}:
                raise ValueError(f"{mode} evidence resolves literal target {target} to another IP")
    for target in allowed["targets"]:
        if {canonical_ip(ip) for ip in allowed["resolvedIPs"][target]} != {
                canonical_ip(ip) for ip in denied["resolvedIPs"][target]}:
            raise ValueError(f"allowed and denied probes resolved different IPs for {target}")
    if allowed["caSHA256"] != denied["caSHA256"]:
        raise ValueError("allowed and denied probes used different CA file contents")
    # Offline verification may have only the declaration and evidence files.
    # If the declared CA file is present, reject evidence for a rotated bundle.
    if os.path.isfile(c["caFile"]) and ca_sha256(c) != allowed["caSHA256"]:
        raise ValueError("local CA file differs from live probe evidence; rerun both probes")
    times = [datetime.datetime.fromisoformat(record["utc"]) for record in (allowed, denied)]
    if any(value.tzinfo is None for value in times) or abs((times[0] - times[1]).total_seconds()) > 300:
        raise ValueError("live probes must be within five minutes")
    now = datetime.datetime.now(datetime.timezone.utc)
    for value in times:
        age = now - value
        if age > datetime.timedelta(minutes=10):
            raise ValueError("live probe evidence is older than ten minutes; rerun both probes")
        if age < -datetime.timedelta(minutes=1):
            raise ValueError("live probe evidence is more than one minute in the future; check host clocks")
    if any(not isinstance(record.get("probeHost"), str) or not record["probeHost"]
           for record in (allowed, denied)):
        raise ValueError("live probe evidence lacks a probe host name")
    if allowed["probeHost"] == denied["probeHost"]:
        raise ValueError("allowed and denied probes must run on distinct hosts")
    networks = [ipaddress.ip_network(value, strict=False) for value in c["allowedSources"]]
    for record, permitted in ((allowed, True), (denied, False)):
        for source in record["sourceIPs"].values():
            if any(canonical_ip(source) in network for network in networks) != permitted:
                raise ValueError(f"{record['mode']} source {source} has wrong allowedSources classification")


def verify_cluster(configs, records):
    if len(configs) != 2 or len(records) != 2:
        raise ValueError("exactly two nodes and their allowed/denied evidence are required")
    if any(validate(config) for config in configs):
        raise ValueError("both node declarations must pass topology validation")
    for config, (allowed, denied) in zip(configs, records):
        verify_pair(config, allowed, denied)
    for index, mode in ((0, "allowed"), (1, "denied")):
        if records[0][index]["probeHost"] != records[1][index]["probeHost"]:
            raise ValueError(f"both nodes must be probed from the same {mode} host")
    if set(records[0][0]["tenantNamespaces"]) != set(records[1][0]["tenantNamespaces"]):
        raise ValueError("two nodes were probed with different tenant namespaces")
    for field in ("tlsHTTP", "tlsGRPC", "backendExternalHost"):
        if configs[0].get(field) == configs[1].get(field):
            raise ValueError(f"two nodes share {field}; distinct network endpoints are required")
    if canonical_ip(configs[0]["backendExternalHost"]) == canonical_ip(configs[1]["backendExternalHost"]):
        raise ValueError("two nodes share backendExternalHost; distinct network endpoints are required")
    entrance_ips = [set(), set()]
    for index in range(2):
        for field in ("tlsHTTP", "tlsGRPC"):
            entrance_ips[index].update(canonical_ip(ip) for ip in records[index][0]["resolvedIPs"][configs[index][field]])
    if entrance_ips[0] & entrance_ips[1]:
        raise ValueError("two nodes' TLS entrances resolve to a shared IP address")
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
    parser.add_argument("--login-token-file", action="append", default=[], metavar="FILE",
                        help="SDK token file for a valid TLS login; --probe requires two distinct tokens")
    parser.add_argument("--grpc-probe-bin", metavar="PATH",
                        help="built nacos-native-grpc-probe binary; required with --probe")
    parser.add_argument("--tenant-namespace", action="append", default=[], metavar="ID",
                        help="namespace ID paired by order with --login-token-file; --probe requires two distinct IDs")
    parser.add_argument("--verify-pair", nargs=2, metavar=("ALLOWED", "DENIED"),
                        help="require same-config, distinct-source live probes within five minutes")
    parser.add_argument("--verify-cluster", nargs=5,
                        metavar=("NODE_B_CONFIG", "A_ALLOWED", "A_DENIED", "B_ALLOWED", "B_DENIED"),
                        help="require paired live evidence for two distinct nodes within five minutes")
    args = parser.parse_args()
    if args.probe and args.probe_denied:
        parser.error("--probe and --probe-denied are mutually exclusive")
    if args.login_token_file and not args.probe:
        parser.error("--login-token-file requires --probe")
    if args.probe and len(args.login_token_file) != 2:
        parser.error("--probe requires two --login-token-file arguments")
    if args.probe and (len(args.tenant_namespace) != 2 or not all(args.tenant_namespace)
                       or args.tenant_namespace[0] == args.tenant_namespace[1]):
        parser.error("--probe requires two distinct --tenant-namespace arguments")
    if args.tenant_namespace and not args.probe:
        parser.error("--tenant-namespace requires --probe")
    if args.probe and not args.grpc_probe_bin:
        parser.error("--probe requires --grpc-probe-bin")
    if args.grpc_probe_bin and not args.probe:
        parser.error("--grpc-probe-bin requires --probe")
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
    resolved = {}
    certificates = {}
    if args.probe:
        try:
            ca_before = ca_sha256(config)
            tokens = []
            for path in args.login_token_file:
                with open(path, encoding="utf-8") as stream:
                    token = stream.read().rstrip("\r\n")
                if not token or "\n" in token or "\r" in token:
                    raise ValueError(f"{path}: expected one nonempty SDK token line")
                tokens.append(token)
            if tokens[0] == tokens[1]:
                raise ValueError("two distinct SDK login tokens are required")
            sources = source_ips(config, "allowed", resolved)
            certificates = probe(config, resolved, tokens, args.tenant_namespace)
            host, port = split_address(config["tlsGRPC"])
            for address in resolved[config["tlsGRPC"]]:
                target = net_address(address, port)
                completed = subprocess.run(
                    [args.grpc_probe_bin, "-address", target, "-server-name", host,
                     "-ca-file", config["caFile"],
                     "-tenant-a-namespace", args.tenant_namespace[0],
                     "-tenant-b-namespace", args.tenant_namespace[1],
                     "-tenant-a-token-file", args.login_token_file[0],
                     "-tenant-b-token-file", args.login_token_file[1]], capture_output=True, text=True,
                    timeout=12, check=False,
                )
                expected = ("PASS: anonymous gRPC denied; two tenant reads and subscriptions allowed; "
                            "cross-tenant reads and subscriptions denied")
                if completed.returncode != 0 or completed.stdout.strip() != expected:
                    raise ValueError(f"two-tenant gRPC isolation failed for {target}: probe exit {completed.returncode}")
            check_probe_dns(config, resolved)
            # An approved SDK source must also be unable to bypass TLS and
            # reach the plaintext Grayling backend on the sidecar host.
            probe_closed_targets(backend_targets(config), resolved,
                                 label="allowed source reached plaintext backend")
            if ca_sha256(config) != ca_before:
                raise ValueError("CA file changed during allowed probe")
        except (OSError, ValueError, subprocess.TimeoutExpired) as exc:
            print("ALERT: live probe failed:", exc, file=sys.stderr)
            return 1
    if args.probe_denied:
        try:
            ca_before = ca_sha256(config)
            sources = source_ips(config, "denied", resolved)
            probe_denied(config, resolved)
            if ca_sha256(config) != ca_before:
                raise ValueError("CA file changed during denied-source probe")
        except (OSError, ValueError) as exc:
            print("ALERT: denied-source probe failed:", exc, file=sys.stderr)
            return 1
    mode = "denied" if args.probe_denied else "allowed" if args.probe else "declaration"
    if args.evidence:
        output = evidence(config, mode, sources, resolved,
                          valid_login_count=len(tokens) if args.probe else 0,
                          ca_digest=ca_before if args.probe or args.probe_denied else None,
                          certificates=certificates,
                          tenant_namespaces=args.tenant_namespace)
        try:
            write_evidence(args.evidence, output)
        except FileExistsError:
            print("ALERT: evidence path already exists; use a new filename for each run", file=sys.stderr)
            return 1
    print("PASS: declared TLS/L4 topology" + (" and live TLS/HTTP probes" if args.probe else " and denied-source TCP probes" if args.probe_denied else ""))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
