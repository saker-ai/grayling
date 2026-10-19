-- v10: independent Applied/Rejected catalog and immutable source receipts.
CREATE TABLE agent_activation_report_heads (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, revision numeric(20,0) NOT NULL CHECK(revision>=0 AND revision<=18446744073709551615),
 PRIMARY KEY(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_activations(workspace_id,environment_id,service_key,generation)
);
CREATE TABLE agent_activation_reports (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, instance_id text NOT NULL, command_id text NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('applied','rejected')),
 revision numeric(20,0) NOT NULL CHECK(revision>0 AND revision<=18446744073709551615),
 report_hash bytea NOT NULL CHECK(octet_length(report_hash)=32), report bytea NOT NULL,
 received_at timestamptz NOT NULL, receipt bytea NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,instance_id),
 UNIQUE(workspace_id,environment_id,service_key,generation,revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_activation_report_heads(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,instance_id) REFERENCES agent_activation_sources(workspace_id,environment_id,service_key,generation,instance_id),
 FOREIGN KEY(workspace_id,environment_id,service_key,command_id) REFERENCES agent_activation_sources(workspace_id,environment_id,service_key,command_id)
);
