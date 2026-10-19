-- Expanded-only v3: management mutations, durable dedup and same-transaction audit.
CREATE TABLE agent_admin_operations (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 actor_id text NOT NULL, operation_id text NOT NULL, action text NOT NULL,
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 result bytea NOT NULL DEFAULT ''::bytea, expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,actor_id,operation_id)
);
CREATE TABLE agent_admin_audit (
 id bigserial PRIMARY KEY, occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 actor_id text NOT NULL, actor_role text NOT NULL, operation_id text NOT NULL,
 action text NOT NULL, object_id text NOT NULL, result bytea NOT NULL
);
CREATE INDEX agent_admin_audit_scope ON agent_admin_audit(workspace_id,environment_id,service_key,id);
