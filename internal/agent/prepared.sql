-- v7: directed Prepare intents and immutable Prepared receipts.
CREATE TABLE agent_prepare_intents (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, command_id text NOT NULL,
 principal_id text NOT NULL, operation_id text NOT NULL,
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 target_revision numeric(20,0) NOT NULL CHECK(target_revision>0 AND target_revision<=18446744073709551615),
 instance_id text NOT NULL, boot_id text NOT NULL,
 credential_id text NOT NULL REFERENCES agent_credentials(id), target_principal_id text NOT NULL, agent_id text NOT NULL,
 lease_id text NOT NULL, lease_epoch numeric(20,0) NOT NULL CHECK(lease_epoch>0 AND lease_epoch<=18446744073709551615),
 descriptor bytea NOT NULL, created_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,command_id),
 UNIQUE(workspace_id,environment_id,service_key,principal_id,operation_id),
 UNIQUE(workspace_id,environment_id,service_key,command_id),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_releases(workspace_id,environment_id,service_key,generation),
 CHECK(expires_at>created_at)
);
CREATE TABLE agent_prepare_heads (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, instance_id text NOT NULL,
 revision numeric(20,0) NOT NULL CHECK(revision>0 AND revision<=18446744073709551615), command_id text NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,instance_id),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,command_id) REFERENCES agent_prepare_intents(workspace_id,environment_id,service_key,generation,command_id)
);
CREATE TABLE agent_prepared_heads (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, revision numeric(20,0) NOT NULL CHECK(revision>=0 AND revision<=18446744073709551615),
 PRIMARY KEY(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_releases(workspace_id,environment_id,service_key,generation)
);
CREATE TABLE agent_prepared_evidence (
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
