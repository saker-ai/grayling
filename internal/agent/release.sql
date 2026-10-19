-- v6: durable release generation, immutable intent, state and operation receipts.
CREATE TABLE agent_release_heads (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL CHECK(generation>=0 AND generation<=18446744073709551615),
 PRIMARY KEY(workspace_id,environment_id,service_key)
);
CREATE TABLE agent_releases (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL CHECK(generation>0 AND generation<=18446744073709551615),
 release_id text NOT NULL, control_plane_epoch bigint NOT NULL CHECK(control_plane_epoch>0),
 definition bytea NOT NULL, definition_sha256 bytea NOT NULL CHECK(octet_length(definition_sha256)=32),
 state integer NOT NULL CHECK(state BETWEEN 1 AND 12),
 revision numeric(20,0) NOT NULL CHECK(revision>0 AND revision<=18446744073709551615),
 created_at timestamptz NOT NULL, prepare_deadline timestamptz NOT NULL, resources_expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation),
 UNIQUE(workspace_id,environment_id,service_key,release_id),
 CHECK(prepare_deadline>created_at AND prepare_deadline<=resources_expires_at)
);
CREATE TABLE agent_release_operations (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 principal_id text NOT NULL, operation_id text NOT NULL,
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 result bytea NOT NULL, expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,principal_id,operation_id)
);
CREATE INDEX agent_release_operations_expiry ON agent_release_operations(expires_at);
CREATE TABLE agent_release_events (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, revision numeric(20,0) NOT NULL,
 from_state integer NOT NULL, to_state integer NOT NULL,
 actor_id text NOT NULL, operation_id text NOT NULL, reason text NOT NULL,
 recorded_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_releases(workspace_id,environment_id,service_key,generation)
);

-- Retained release intent pins its exact immutable signed dependencies.
CREATE TABLE agent_release_resource_refs (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 generation numeric(20,0) NOT NULL, resource_key text NOT NULL, revision numeric(20,0) NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,generation,resource_key,revision),
 FOREIGN KEY(workspace_id,environment_id,service_key,generation) REFERENCES agent_releases(workspace_id,environment_id,service_key,generation),
 FOREIGN KEY(workspace_id,environment_id,service_key,resource_key,revision) REFERENCES agent_resources(workspace_id,environment_id,service_key,resource_key,revision)
);
CREATE INDEX agent_release_resource_refs_resource ON agent_release_resource_refs(workspace_id,environment_id,service_key,resource_key,revision);
