#!/usr/bin/env bash
# Disposable Docker PG + Redis drill; source writers must already be stopped.
set -euo pipefail
umask 077
: "${GOLAB_BIN:?}"
: "${GOLAB_DRILL_SOURCE_PG_DSN:?}"
: "${GOLAB_DRILL_TARGET_PG_DSN:?}"
: "${GOLAB_DRILL_PG_CONTAINER:?}"
: "${GOLAB_DRILL_PG_USER:?}"
: "${GOLAB_DRILL_SOURCE_DB:?}"
: "${GOLAB_DRILL_TARGET_DB:?}"
: "${GOLAB_DRILL_TARGET_REDIS_CONTAINER:?}"
: "${GOLAB_DRILL_SOURCE_REDIS_ADDR:?}"
: "${GOLAB_DRILL_TARGET_REDIS_ADDR:?}"
: "${GOLAB_AUTH_SECRET_FILE:?}"
: "${GOLAB_DRILL_DISPOSABLE_TARGET:?set YES for a disposable empty target}"
test "$GOLAB_DRILL_DISPOSABLE_TARGET" = YES
test "$GOLAB_DRILL_SOURCE_DB" != "$GOLAB_DRILL_TARGET_DB"
[[ "$GOLAB_DRILL_SOURCE_DB" =~ ^[a-zA-Z_][a-zA-Z_0-9]*$ ]]
[[ "$GOLAB_DRILL_TARGET_DB" =~ ^[a-zA-Z_][a-zA-Z_0-9]*$ ]]
if docker inspect "$GOLAB_DRILL_TARGET_REDIS_CONTAINER" >/dev/null 2>&1; then
  echo 'target Redis container name already exists' >&2; exit 1
fi
test "$(docker exec "$GOLAB_DRILL_PG_CONTAINER" psql -U "$GOLAB_DRILL_PG_USER" -d "$GOLAB_DRILL_TARGET_DB" -Atc "select count(*) from pg_tables where schemaname='public'")" = 0

work=$(mktemp -d)
drill_pid=
drill_pid_b=
cleanup() {
  status=$?
  if test "$status" != 0 && test -f "$work/restored-server.log"; then
    sed -E 's/glab_[A-Za-z0-9_-]+/[redacted]/g' "$work/restored-server.log" | tail -20 >&2
  fi
  if test -n "$drill_pid"; then kill -INT "$drill_pid" 2>/dev/null || true; wait "$drill_pid" 2>/dev/null || true; fi
  if test -n "$drill_pid_b"; then kill -INT "$drill_pid_b" 2>/dev/null || true; wait "$drill_pid_b" 2>/dev/null || true; fi
  docker rm -f "$GOLAB_DRILL_TARGET_REDIS_CONTAINER" >/dev/null 2>&1 || true
  rm -r "$work"
}
trap cleanup EXIT
printf 'source_sha=%s\n' "$(git rev-parse HEAD)"
printf 'backup_start=%s\n' "$(date -u +%FT%TZ)"
"$GOLAB_BIN" -pg "$GOLAB_DRILL_SOURCE_PG_DSN" -redis "$GOLAB_DRILL_SOURCE_REDIS_ADDR" \
  -auth-secret-file "$GOLAB_AUTH_SECRET_FILE" -gonacos-addr 127.0.0.1:0 \
  -gonacos-grpc-addr 127.0.0.1:0 -write-recovery-point "$work/point.json"
# redis-cli --rdb uses a full-sync snapshot streamed to the local staging
# directory; the source Redis need not have free disk space or expose its RDB.
redis_host=${GOLAB_DRILL_SOURCE_REDIS_ADDR%:*}
redis_port=${GOLAB_DRILL_SOURCE_REDIS_ADDR##*:}
redis_image=${GOLAB_DRILL_REDIS_IMAGE:-redis:7.4-alpine}
docker run --rm --user "$(id -u):$(id -g)" --network host -v "$work:/backup" "$redis_image" \
  redis-cli -h "$redis_host" -p "$redis_port" --rdb /backup/dump.rdb
test -s "$work/dump.rdb"
docker exec "$GOLAB_DRILL_PG_CONTAINER" pg_dump -U "$GOLAB_DRILL_PG_USER" -Fc -d "$GOLAB_DRILL_SOURCE_DB" > "$work/postgres.dump"
sha256sum "$work"/point.json "$work"/dump.rdb "$work"/postgres.dump
printf 'backup_complete=%s\n' "$(date -u +%FT%TZ)"
printf 'restore_start=%s\n' "$(date -u +%FT%TZ)"
docker run -d --rm --user "$(id -u):$(id -g)" --name "$GOLAB_DRILL_TARGET_REDIS_CONTAINER" \
  -p "$GOLAB_DRILL_TARGET_REDIS_ADDR:6379" -v "$work:/data" "$redis_image" >/dev/null
for attempt in {1..30}; do
  if test "$(docker exec "$GOLAB_DRILL_TARGET_REDIS_CONTAINER" redis-cli PING 2>/dev/null)" = PONG; then break; fi
  sleep 1
done
test "$(docker exec "$GOLAB_DRILL_TARGET_REDIS_CONTAINER" redis-cli EXISTS gonacos:snapshot)" = 1
docker exec -i "$GOLAB_DRILL_PG_CONTAINER" pg_restore -U "$GOLAB_DRILL_PG_USER" -d "$GOLAB_DRILL_TARGET_DB" --exit-on-error --no-owner --no-acl < "$work/postgres.dump"
"$GOLAB_BIN" -pg "$GOLAB_DRILL_TARGET_PG_DSN" -redis "$GOLAB_DRILL_TARGET_REDIS_ADDR" \
  -auth-secret-file "$GOLAB_AUTH_SECRET_FILE" -gonacos-addr 127.0.0.1:0 \
  -gonacos-grpc-addr 127.0.0.1:0 -verify-recovery-point "$work/point.json"
printf 'verify_complete=%s\n' "$(date -u +%FT%TZ)"

if test -n "${GOLAB_DRILL_SDK_A_TOKEN_FILE:-}"; then
  : "${GOLAB_DRILL_SDK_B_TOKEN_FILE:?}"
  : "${GOLAB_DRILL_HTTP_ADDR:?}"
  : "${GOLAB_DRILL_A_VALUE:?}"
  : "${GOLAB_DRILL_B_VALUE:?}"
  "$GOLAB_BIN" -pg "$GOLAB_DRILL_TARGET_PG_DSN" -redis "$GOLAB_DRILL_TARGET_REDIS_ADDR" \
    -auth-secret-file "$GOLAB_AUTH_SECRET_FILE" -gonacos-addr 127.0.0.1:0 \
    -gonacos-grpc-addr 127.0.0.1:0 -addr "$GOLAB_DRILL_HTTP_ADDR" \
    -rollout-interval 0 -reconcile-interval 0 -recovery-point "$work/point.json" \
    > "$work/restored-server.log" 2>&1 &
  drill_pid=$!
  if test -n "${GOLAB_DRILL_HTTP_ADDR_B:-}"; then
    "$GOLAB_BIN" -pg "$GOLAB_DRILL_TARGET_PG_DSN" -redis "$GOLAB_DRILL_TARGET_REDIS_ADDR" \
      -auth-secret-file "$GOLAB_AUTH_SECRET_FILE" -gonacos-addr 127.0.0.1:0 \
      -gonacos-grpc-addr 127.0.0.1:0 -addr "$GOLAB_DRILL_HTTP_ADDR_B" \
      -rollout-interval 0 -reconcile-interval 0 -recovery-point "$work/point.json" \
      > "$work/restored-server-b.log" 2>&1 &
    drill_pid_b=$!
  fi
  python3 - <<'PY'
import json,os,time,urllib.request
base='http://'+os.environ['GOLAB_DRILL_HTTP_ADDR']
base_b='http://'+os.environ['GOLAB_DRILL_HTTP_ADDR_B'] if os.environ.get('GOLAB_DRILL_HTTP_ADDR_B') else None
def request(path, token, body=None, endpoint=base):
    headers={'Authorization':'Bearer '+token}
    if body is not None: headers['Content-Type']='application/json'
    req=urllib.request.Request(endpoint+path, data=json.dumps(body).encode() if body else None, headers=headers)
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(req, timeout=2) as resp: return json.load(resp)
for _ in range(40):
    try:
        request('/api/v1/health','')
        if base_b: request('/api/v1/health','',endpoint=base_b)
        break
    except Exception: time.sleep(.25)
else: raise RuntimeError('restored server not ready')
if os.environ.get('GOLAB_DRILL_COLLECT_TOKEN_FILE'):
    collect=open(os.environ['GOLAB_DRILL_COLLECT_TOKEN_FILE']).read().strip()
    event=json.load(open(os.environ['GOLAB_DRILL_DUPLICATE_EVENT_FILE']))
    result=request('/api/v1/collect',collect,event)
    assert result['accepted']==0 and result['duplicates']==1, 'event dedup lost after restore'
    owner=open(os.environ['GOLAB_DRILL_OWNER_TOKEN_FILE']).read().strip()
    exp=open(os.environ['GOLAB_DRILL_EXP_ID_FILE']).read().strip()
    before=json.load(open(os.environ['GOLAB_DRILL_REPORT_BEFORE_FILE']))
    after=request('/api/v1/admin/experiments/'+exp+'/report',owner)
    for field in ('perVariant','srm','decision','registrationVersion','primaryError'):
        assert before.get(field)==after.get(field), f'historical report drift: {field}'
    print('event_dedup_verified=1 historical_report_verified=1')
for label in ('A','B'):
    token=open(os.environ[f'GOLAB_DRILL_SDK_{label}_TOKEN_FILE']).read().strip()
    expected=os.environ[f'GOLAB_DRILL_{label}_VALUE']
    config=request('/api/v1/sdk/config',token)
    flag=config['flags']['feature']
    assert expected in json.dumps(flag), f'{label} config missing own value'
    other=os.environ['GOLAB_DRILL_B_VALUE' if label=='A' else 'GOLAB_DRILL_A_VALUE']
    assert other not in json.dumps(flag), f'{label} config leaked other tenant'
    result=request('/api/v1/sdk/eval',token,{'flagKey':'feature','context':{'targetingKey':'probe','attributes':{'user_id':'probe'}}})
    assert result['result']['variationType'], f'{label} eval failed'
    if base_b:
        other_config=request('/api/v1/sdk/config',token,endpoint=base_b)
        assert other_config['flags']['feature']==config['flags']['feature'], f'{label} replica config drift'
        other_result=request('/api/v1/sdk/eval',token,{'flagKey':'feature','context':{'targetingKey':'probe','attributes':{'user_id':'probe'}}},endpoint=base_b)
        assert (other_result['value'],other_result['manifestRevision'])==(result['value'],result['manifestRevision']), f'{label} replica eval drift'
print('tenant_config_eval_verified=2')
if base_b: print('restored_replicas_verified=2')
PY
  printf 'serving_verified=%s\n' "$(date -u +%FT%TZ)"
  kill -INT "$drill_pid"
  wait "$drill_pid"
  drill_pid=
  if test -n "$drill_pid_b"; then
    kill -INT "$drill_pid_b"
    wait "$drill_pid_b"
    drill_pid_b=
  fi
fi

printf 'wrong_secret\n' > "$work/wrong-secret"
if "$GOLAB_BIN" -pg "$GOLAB_DRILL_TARGET_PG_DSN" -redis "$GOLAB_DRILL_TARGET_REDIS_ADDR" \
  -auth-secret-file "$work/wrong-secret" -gonacos-addr 127.0.0.1:0 \
  -gonacos-grpc-addr 127.0.0.1:0 -verify-recovery-point "$work/point.json" > "$work/wrong-secret.log" 2>&1; then
  echo 'wrong secret unexpectedly passed' >&2; exit 1
fi
printf 'wrong_secret_rejected=1\n'
docker exec "$GOLAB_DRILL_PG_CONTAINER" psql -U "$GOLAB_DRILL_PG_USER" -d "$GOLAB_DRILL_TARGET_DB" \
  -v ON_ERROR_STOP=1 -q -c 'UPDATE meta SET next_counter=next_counter+1 WHERE id=1' >/dev/null
if "$GOLAB_BIN" -pg "$GOLAB_DRILL_TARGET_PG_DSN" -redis "$GOLAB_DRILL_TARGET_REDIS_ADDR" \
  -auth-secret-file "$GOLAB_AUTH_SECRET_FILE" -gonacos-addr 127.0.0.1:0 \
  -gonacos-grpc-addr 127.0.0.1:0 -verify-recovery-point "$work/point.json" > "$work/changed-pg.log" 2>&1; then
  echo 'changed PostgreSQL unexpectedly passed' >&2; exit 1
fi
printf 'changed_postgres_rejected=1\n'
# The target remains disposable; undo this one injection so the next case
# tests Redis Snapshot loss rather than the already-detected PG mismatch.
docker exec "$GOLAB_DRILL_PG_CONTAINER" psql -U "$GOLAB_DRILL_PG_USER" -d "$GOLAB_DRILL_TARGET_DB" \
  -v ON_ERROR_STOP=1 -q -c 'UPDATE meta SET next_counter=next_counter-1 WHERE id=1' >/dev/null
docker exec "$GOLAB_DRILL_TARGET_REDIS_CONTAINER" redis-cli DEL gonacos:snapshot >/dev/null
if "$GOLAB_BIN" -pg "$GOLAB_DRILL_TARGET_PG_DSN" -redis "$GOLAB_DRILL_TARGET_REDIS_ADDR" \
  -auth-secret-file "$GOLAB_AUTH_SECRET_FILE" -gonacos-addr 127.0.0.1:0 \
  -gonacos-grpc-addr 127.0.0.1:0 -verify-recovery-point "$work/point.json" > "$work/missing-snapshot.log" 2>&1; then
  echo 'missing snapshot unexpectedly passed' >&2; exit 1
fi
printf 'missing_snapshot_rejected=1\n'
echo 'The target now contains the injected failure; discard it and restore from the same archive before serving traffic.'
