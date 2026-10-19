CREATE TABLE agent_resource_heads (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL, resource_key text NOT NULL,
 revision numeric(20,0) NOT NULL CHECK(revision>=0 AND revision<=18446744073709551615),
 PRIMARY KEY(workspace_id,environment_id,service_key,resource_key)
);
CREATE TABLE agent_resources (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL, resource_key text NOT NULL,
 revision numeric(20,0) NOT NULL CHECK(revision>0 AND revision<=18446744073709551615),
 body bytea NOT NULL, sha256 bytea NOT NULL CHECK(octet_length(sha256)=32), expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,resource_key,revision)
);
CREATE TABLE agent_resource_operations (
 workspace_id text NOT NULL, environment_id text NOT NULL, service_key text NOT NULL,
 principal_id text NOT NULL, operation_id text NOT NULL,
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32), resource_key text NOT NULL,
 revision numeric(20,0) NOT NULL CHECK(revision>=0 AND revision<=18446744073709551615), expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,principal_id,operation_id)
);
CREATE INDEX agent_resource_operations_expiry ON agent_resource_operations(expires_at);
