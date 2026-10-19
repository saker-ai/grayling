-- v9: exact signed activation views and immutable required source commands.
CREATE TABLE agent_activations (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, release_revision numeric(20,0) NOT NULL,
 resource_key text NOT NULL, resource_revision numeric(20,0) NOT NULL,
 created_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,release_revision) REFERENCES agent_release_events(workspace_id,environment_id,service_key,generation,revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,resource_key,resource_revision) REFERENCES agent_resources(workspace_id,environment_id,service_key,resource_key,revision),
 CHECK(expires_at>created_at)
);
CREATE TABLE agent_activation_sources (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, instance_id text NOT NULL, command_id text NOT NULL,
 credential_id text NOT NULL REFERENCES agent_credentials(id), prepare_command_id text NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,instance_id),
 UNIQUE(workspace_id,environment_id,service_key,command_id),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_activations(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation,prepare_command_id) REFERENCES agent_prepare_intents(workspace_id,environment_id,service_key,generation,command_id)
);
