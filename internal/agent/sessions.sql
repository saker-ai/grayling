-- Expanded-only Agent schema. Executed under the Agent migration advisory lock.
CREATE TABLE IF NOT EXISTS agent_schema (id boolean PRIMARY KEY CHECK(id), version integer NOT NULL, control_plane_epoch bigint NOT NULL DEFAULT 1 CHECK(control_plane_epoch>0));
INSERT INTO agent_schema(id,version) VALUES(true,0) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS agent_scope_limits (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 max_connections integer NOT NULL CHECK(max_connections>0),
 PRIMARY KEY(workspace_id,environment_id,service_key)
);
CREATE TABLE IF NOT EXISTS agent_credentials (
 id text PRIMARY KEY, secret_hash bytea UNIQUE NOT NULL CHECK(octet_length(secret_hash)=32),
 principal_id text NOT NULL, workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 agent_id text NOT NULL, instance_id text NOT NULL, boot_id text NOT NULL, takeover_from_boot_id text NOT NULL,
 permissions bigint NOT NULL CHECK(permissions>0), expires_at timestamptz NOT NULL, revoked boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS agent_sessions (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL, instance_id text NOT NULL,
 agent_id text NOT NULL, boot_id text NOT NULL, credential_id text NOT NULL REFERENCES agent_credentials(id),
 session_id text NOT NULL UNIQUE, connection_epoch bigint NOT NULL CHECK(connection_epoch>0),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,instance_id)
);
CREATE INDEX IF NOT EXISTS agent_sessions_expiry ON agent_sessions(workspace_id,environment_id,service_key,expires_at);
UPDATE agent_schema SET version=1 WHERE id=true AND version=0;
