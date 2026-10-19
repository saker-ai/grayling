-- v8: negative preparation receipts and immutable capacity decisions.
CREATE TABLE agent_prepare_rejections (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, command_id text NOT NULL,
 revision numeric(20,0) NOT NULL CHECK(revision>0 AND revision<=18446744073709551615),
 report_hash bytea NOT NULL CHECK(octet_length(report_hash)=32), report bytea NOT NULL,
 received_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,command_id),
 UNIQUE(workspace_id,environment_id,service_key,generation,revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_prepared_heads(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,command_id) REFERENCES agent_prepare_intents(workspace_id,environment_id,service_key,generation,command_id),
 CHECK(expires_at>received_at)
);
CREATE TABLE agent_preparation_decisions (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, release_revision numeric(20,0) NOT NULL,
 decision bytea NOT NULL, sha256 bytea NOT NULL CHECK(octet_length(sha256)=32),
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,release_revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,release_revision) REFERENCES agent_release_events(workspace_id,environment_id,service_key,generation,revision)
);
