"""Independent Python producer for the public, TEST-ONLY Ed25519 fixture.

Requires cryptography==41.0.7. No production key is read or written.
Run --check to verify the committed fixture without modifying it.
"""
import argparse
import hashlib
import json
import pathlib
import struct
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


def text(value):
    data = value.encode('utf-8')
    return struct.pack('>H', len(data)) + data


def fixture():
    payload = b'test policy bytes'
    claims = (b'GAR1' + text('workspace-1') + text('env-1') + text('checkout')
              + struct.pack('>I', 1) + text('policy')
              + struct.pack('>QQ', 1, 9007199254740993)
              + hashlib.sha256(b'bundle').digest()
              + struct.pack('>QQ', 1699999940000, 1700000060000)
              + text('test-key-1') + hashlib.sha256(payload).digest())
    key = Ed25519PrivateKey.from_private_bytes(b'\x42' * 32)
    signed = b'grayling.agent.v1.signed-resource\0' + claims
    signature = key.sign(signed)
    key.public_key().verify(signature, signed)
    return dict(description='Public TEST-ONLY vector, produced independently by Python cryptography 41.0.7',
                claimsHex=claims.hex(), payloadHex=payload.hex(), signatureHex=signature.hex(),
                publicKeyHex=key.public_key().public_bytes(serialization.Encoding.Raw,
                                                         serialization.PublicFormat.Raw).hex(),
                keyId='test-key-1', revision='9007199254740993', nowUnixMs='1700000000000')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    path = pathlib.Path(__file__).resolve().parent.parent / 'test/agent/fixtures/signed-policy-v1.json'
    expected = fixture()
    if args.check:
        if json.loads(path.read_text()) != expected:
            raise SystemExit('signed resource fixture differs')
        print('Python Ed25519 fixture matches')
    else:
        path.write_text(json.dumps(expected, indent=2) + '\n')


if __name__ == '__main__':
    main()
