#!/usr/bin/env bash
# Quiesced, embedded-gonacos recovery set. See docs/联合备份恢复.md.
set -euo pipefail
umask 077
: "${GOLAB_BIN:?set GOLAB_BIN to the built golab binary}"
: "${GOLAB_PG_DSN:?set GOLAB_PG_DSN}"
: "${GOLAB_GONACOS_DATA:?set GOLAB_GONACOS_DATA}"
: "${GOLAB_AUTH_SECRET_FILE:?set GOLAB_AUTH_SECRET_FILE}"
: "${GOLAB_BACKUP_OUT:?set GOLAB_BACKUP_OUT to a new .tar.gpg path}"
: "${GOLAB_GPG_RECIPIENT:?set GOLAB_GPG_RECIPIENT to an escrow public key}"
: "${GOLAB_GPG_SIGNER:?set GOLAB_GPG_SIGNER to the backup signing key}"
test ! -e "$GOLAB_BACKUP_OUT" || { echo 'backup output already exists' >&2; exit 1; }
test -f "$GOLAB_AUTH_SECRET_FILE" || { echo 'missing auth secret' >&2; exit 1; }
work=$(mktemp -d)
out_tmp=$(mktemp "${GOLAB_BACKUP_OUT}.XXXXXX")
trap 'rm -rf "$work"; rm -f "$out_tmp"' EXIT
mkdir "$work/set"

# All writers and reconcilers must already be stopped. This command takes a
# checkpoint, shuts gonacos down, and forces its final snapshot to disk.
"$GOLAB_BIN" -pg "$GOLAB_PG_DSN" -gonacos-data "$GOLAB_GONACOS_DATA" \
  -gonacos-addr 127.0.0.1:0 -gonacos-grpc-addr 127.0.0.1:0 \
  -auth-secret-file "$GOLAB_AUTH_SECRET_FILE" -write-recovery-point "$work/set/point.json"
for name in snapshot.json snapshot.json.hmac; do
  test -s "$GOLAB_GONACOS_DATA/$name" || { echo "missing saved gonacos $name" >&2; exit 1; }
  cp "$GOLAB_GONACOS_DATA/$name" "$work/set/$name"
done
cp "$GOLAB_AUTH_SECRET_FILE" "$work/set/auth-secret"

if command -v pg_dump >/dev/null; then
  pg_dump --format=custom --file="$work/set/postgres.dump" "$GOLAB_PG_DSN"
else
  docker run --rm --network host -v "$work/set:/backup" postgres:16-alpine \
    pg_dump --format=custom --file=/backup/postgres.dump "$GOLAB_PG_DSN"
fi
test -s "$work/set/postgres.dump"
(cd "$work/set" && sha256sum point.json snapshot.json snapshot.json.hmac auth-secret postgres.dump > SHA256SUMS)
tar -C "$work" -cf "$work/bundle.tar" set
gpg --batch --yes --trust-model always --sign --encrypt --local-user "$GOLAB_GPG_SIGNER" \
  --recipient "$GOLAB_GPG_RECIPIENT" \
  --output "$work/bundle.tar.gpg" "$work/bundle.tar"
cp "$work/bundle.tar.gpg" "$out_tmp"
mv "$out_tmp" "$GOLAB_BACKUP_OUT"
echo "encrypted recovery set: $GOLAB_BACKUP_OUT"
