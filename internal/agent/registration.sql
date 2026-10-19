-- Expanded-only v4. Registration governance is authoritative; Naming is a projection.
CREATE TABLE agent_registration_scopes (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 policy bytea NOT NULL, policy_revision numeric(20,0) NOT NULL CHECK(policy_revision>=0 AND policy_revision<=18446744073709551615),
 discovery_revision numeric(20,0) NOT NULL CHECK(discovery_revision>=0 AND discovery_revision<=18446744073709551615),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,environment_id,service_key)
);
CREATE TABLE agent_deployments (
 id text PRIMARY KEY, workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 instance_id text NOT NULL, boot_id text NOT NULL, principal_id text NOT NULL, agent_id text NOT NULL,
 policy_revision numeric(20,0) NOT NULL, descriptor bytea NOT NULL, expires_at timestamptz NOT NULL,
 UNIQUE(workspace_id,environment_id,service_key,instance_id,boot_id,policy_revision)
);
CREATE TABLE agent_instances (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL, instance_id text NOT NULL,
 boot_id text NOT NULL, principal_id text NOT NULL, agent_id text NOT NULL, deployment_id text NOT NULL REFERENCES agent_deployments(id),
 credential_id text NOT NULL REFERENCES agent_credentials(id), descriptor bytea NOT NULL, lease_id text NOT NULL UNIQUE,
 lease_epoch numeric(20,0) NOT NULL CHECK(lease_epoch>0 AND lease_epoch<=18446744073709551615),
 expires_at timestamptz NOT NULL, active boolean NOT NULL,
 policy_revision numeric(20,0) NOT NULL, desired_revision numeric(20,0) NOT NULL, confirmed_revision numeric(20,0) NOT NULL DEFAULT 0,
 PRIMARY KEY(workspace_id,environment_id,service_key,instance_id)
);
CREATE INDEX agent_instances_expiry ON agent_instances(workspace_id,environment_id,service_key,expires_at) WHERE active;
CREATE TABLE agent_instance_operations (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL, principal_id text NOT NULL, operation_id text NOT NULL,
 action text NOT NULL, request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32), result bytea NOT NULL DEFAULT ''::bytea,
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,principal_id,operation_id)
);
CREATE INDEX agent_instance_operations_expiry ON agent_instance_operations(expires_at);
CREATE TABLE agent_naming_intents (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 instance_id text NOT NULL, lease_id text NOT NULL, lease_epoch numeric(20,0) NOT NULL,
 revision numeric(20,0) NOT NULL, descriptor bytea NOT NULL, present boolean NOT NULL,
 expires_at timestamptz NOT NULL, confirmed boolean NOT NULL DEFAULT false,
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,environment_id,service_key,lease_id,revision)
);
CREATE INDEX agent_naming_intents_due ON agent_naming_intents(next_attempt_at);
