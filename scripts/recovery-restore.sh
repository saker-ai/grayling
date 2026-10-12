#!/usr/bin/env bash
# Restore into an empty isolated PostgreSQL database and empty destination.
set -euo pipefail
umask 077
: "${GOLAB_BIN:?set GOLAB_BIN to the built golab binary}"
: "${GOLAB_PG_DSN:?set GOLAB_PG_DSN to an empty target database}"
: "${GOLAB_BACKUP_IN:?set GOLAB_BACKUP_IN}"
: "${GOLAB_RESTORE_DIR:?set GOLAB_RESTORE_DIR to a new directory}"
: "${GOLAB_GPG_SIGNER_FINGERPRINT:?set expected backup signing-key fingerprint}"
test ! -e "$GOLAB_RESTORE_DIR" || { echo 'restore directory already exists' >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gpg --batch --status-file "$work/gpg.status" --decrypt --output "$work/bundle.tar" "$GOLAB_BACKUP_IN"
awk -v fingerprint="$GOLAB_GPG_SIGNER_FINGERPRINT" '$2 == "VALIDSIG" && $3 == fingerprint { found=1 } END { exit !found }' \
  "$work/gpg.status" || { echo 'backup signature missing or signed by an unexpected key' >&2; exit 1; }
tar -C "$work" -xf "$work/bundle.tar"
(cd "$work/set" && sha256sum -c SHA256SUMS)

pg_client() {
  if command -v "$1" >/dev/null; then
    "$@"
  else
    docker run --rm --network host -v "$work/set:/backup" postgres:16-alpine "$@"
  fi
}
tables=$(pg_client psql -At -c "SELECT count(*) FROM pg_tables WHERE schemaname='public'" "$GOLAB_PG_DSN")
test "$tables" = 0 || { echo 'target PostgreSQL public schema is not empty' >&2; exit 1; }
if command -v pg_restore >/dev/null; then
  pg_restore --exit-on-error --no-owner --no-acl --dbname="$GOLAB_PG_DSN" "$work/set/postgres.dump"
else
  pg_client pg_restore --exit-on-error --no-owner --no-acl --dbname="$GOLAB_PG_DSN" /backup/postgres.dump
fi
want=$(jq -r '.migrations | join(",")' "$work/set/point.json")
got=$(pg_client psql -At -c "SELECT convert_from(payload, 'UTF8')::jsonb->>'schemaVersion' FROM database_states WHERE id=1" "$GOLAB_PG_DSN")
test "$got" = "$want" || { echo "schema migration mismatch: restored=$got expected=$want; stop and roll back" >&2; exit 1; }

mkdir -m 700 "$GOLAB_RESTORE_DIR"
mkdir -m 700 "$GOLAB_RESTORE_DIR/gonacos"
install -m 600 "$work/set/snapshot.json" "$GOLAB_RESTORE_DIR/gonacos/snapshot.json"
install -m 600 "$work/set/snapshot.json.hmac" "$GOLAB_RESTORE_DIR/gonacos/snapshot.json.hmac"
install -m 600 "$work/set/auth-secret" "$GOLAB_RESTORE_DIR/auth-secret"
install -m 600 "$work/set/point.json" "$GOLAB_RESTORE_DIR/point.json"
"$GOLAB_BIN" -pg "$GOLAB_PG_DSN" -gonacos-data "$GOLAB_RESTORE_DIR/gonacos" \
  -gonacos-addr 127.0.0.1:0 -gonacos-grpc-addr 127.0.0.1:0 \
  -auth-secret-file "$GOLAB_RESTORE_DIR/auth-secret" \
  -verify-recovery-point "$GOLAB_RESTORE_DIR/point.json"
echo "verified restore: $GOLAB_RESTORE_DIR"
