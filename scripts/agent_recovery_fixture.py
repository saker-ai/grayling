"""Independent TEST-ONLY GRA1 recovery vector; cryptography==41.0.7."""
import argparse
import hashlib
import json
import pathlib
import struct
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


def text(value):
    raw = value.encode('utf-8')
    return struct.pack('>H', len(raw)) + raw


def public(key):
    return key.public_key().public_bytes(serialization.Encoding.Raw,
                                         serialization.PublicFormat.Raw).hex()


def fixture():
    payload = json.dumps(dict(schemaVersion=1, workspace='w', environment='e',
        manifestRevision=1, generatedAt='2023-11-14T22:13:20Z', flags=dict(route=dict(
            schemaVersion=1, key='route', type='string', variations=dict(stable='backend'),
            defaultRule=dict(variation='stable')))), separators=(',', ':')).encode()
    target = (b'GAR1' + text('w') + text('e') + text('s') + struct.pack('>I', 1)
              + text('policy') + struct.pack('>QQ', 2, 1)
              + hashlib.sha256(b'restored-bundle').digest()
              + struct.pack('>QQ', 1699999999000, 1700000600000)
              + text('policy-key') + hashlib.sha256(payload).digest())
    previous = (struct.pack('>QQ', 1, 9007199254740993)
                + hashlib.sha256(b'old-payload').digest()
                + hashlib.sha256(b'old-bundle').digest())
    claims = (b'GRA1' + struct.pack('>I', len(target)) + target + previous
              + hashlib.sha256(b'independent-test-nonce').digest()
              + struct.pack('>QQ', 1699999999000, 1700000300000)
              + text('recovery-key'))
    policy = Ed25519PrivateKey.from_private_bytes(b'\x42' * 32)
    recovery = Ed25519PrivateKey.from_private_bytes(b'\x43' * 32)
    return dict(description='Public TEST-ONLY recovery vector from Python cryptography 41.0.7',
                payloadHex=payload.hex(), targetHex=target.hex(), recoveryHex=claims.hex(),
                resourceSignatureHex=policy.sign(b'grayling.agent.v1.signed-resource\0' + target).hex(),
                recoverySignatureHex=recovery.sign(b'grayling.agent.v1.epoch-recovery\0' + claims).hex(),
                policyPublicHex=public(policy), recoveryPublicHex=public(recovery),
                nowUnixMs='1700000000000')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    path = pathlib.Path(__file__).resolve().parent.parent / 'test/agent/fixtures/recovery-v1.json'
    expected = fixture()
    if args.check:
        if json.loads(path.read_text()) != expected:
            raise SystemExit('recovery fixture differs')
        print('Python independent recovery fixture matches')
    else:
        path.write_text(json.dumps(expected, indent=2) + '\n')


if __name__ == '__main__':
    main()
