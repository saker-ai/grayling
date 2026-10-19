-- v11: immutable artifact identities; agent_activations remains the current head.
CREATE TABLE agent_activation_versions (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, release_revision numeric(20,0) NOT NULL,
 resource_key text NOT NULL, resource_revision numeric(20,0) NOT NULL CHECK(resource_revision>0 AND resource_revision<=18446744073709551615),
 created_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,resource_revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_activations(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,release_revision) REFERENCES agent_release_events(workspace_id,environment_id,service_key,generation,revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,resource_key,resource_revision) REFERENCES agent_resources(workspace_id,environment_id,service_key,resource_key,revision),
 CHECK(expires_at>created_at)
);
INSERT INTO agent_activation_versions SELECT * FROM agent_activations;
ALTER TABLE agent_activation_reports RENAME TO agent_activation_reports_v10;
ALTER TABLE agent_activation_sources RENAME TO agent_activation_sources_v10;
CREATE TABLE agent_activation_sources (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, instance_id text NOT NULL, command_id text NOT NULL,
 credential_id text NOT NULL REFERENCES agent_credentials(id), prepare_command_id text NOT NULL,
 artifact_revision numeric(20,0) NOT NULL CHECK(artifact_revision>0 AND artifact_revision<=18446744073709551615),
 CONSTRAINT agent_activation_sources_v11_pk PRIMARY KEY(workspace_id,environment_id,service_key,generation,artifact_revision,instance_id),
 CONSTRAINT agent_activation_sources_v11_command UNIQUE(workspace_id,environment_id,service_key,command_id),
 CONSTRAINT agent_activation_sources_v11_identity UNIQUE(workspace_id,environment_id,service_key,generation,artifact_revision,instance_id,command_id),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,artifact_revision) REFERENCES agent_activation_versions(workspace_id,environment_id,service_key,generation,resource_revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,prepare_command_id) REFERENCES agent_prepare_intents(workspace_id,environment_id,service_key,generation,command_id)
);
INSERT INTO agent_activation_sources SELECT s.*,a.resource_revision FROM agent_activation_sources_v10 s JOIN agent_activations a USING(workspace_id,environment_id,service_key,generation);
CREATE TABLE agent_activation_reports (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, instance_id text NOT NULL, command_id text NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('applied','rejected')),
 revision numeric(20,0) NOT NULL CHECK(revision>0 AND revision<=18446744073709551615),
 report_hash bytea NOT NULL CHECK(octet_length(report_hash)=32), report bytea NOT NULL,
 received_at timestamptz NOT NULL, receipt bytea NOT NULL,
 artifact_revision numeric(20,0) NOT NULL CHECK(artifact_revision>0 AND artifact_revision<=18446744073709551615),
 CONSTRAINT agent_activation_reports_v11_pk PRIMARY KEY(workspace_id,environment_id,service_key,generation,artifact_revision,instance_id),
 CONSTRAINT agent_activation_reports_v11_revision UNIQUE(workspace_id,environment_id,service_key,generation,revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_activation_report_heads(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,artifact_revision,instance_id,command_id) REFERENCES agent_activation_sources(workspace_id,environment_id,service_key,generation,artifact_revision,instance_id,command_id)
);
INSERT INTO agent_activation_reports SELECT r.*,a.resource_revision FROM agent_activation_reports_v10 r JOIN agent_activations a USING(workspace_id,environment_id,service_key,generation);
DROP TABLE agent_activation_reports_v10,agent_activation_sources_v10;
