-- v5: immutable discovery artifacts, separate from membership revisions.
CREATE TABLE agent_discovery_heads (
 workspace_id text NOT NULL,environment_id text NOT NULL,service_key text NOT NULL,
 bundle_hash bytea NOT NULL CHECK(octet_length(bundle_hash)=32),
 revision numeric(20,0) NOT NULL CHECK(revision>=0 AND revision<=18446744073709551615),
 PRIMARY KEY(workspace_id,environment_id,service_key,bundle_hash)
);
CREATE TABLE agent_discovery_resources (
 workspace_id text NOT NULL,environment_id text NOT NULL,service_key text NOT NULL,
 bundle_hash bytea NOT NULL CHECK(octet_length(bundle_hash)=32),
 revision numeric(20,0) NOT NULL CHECK(revision>0 AND revision<=18446744073709551615),
 membership_revision numeric(20,0) NOT NULL,
 control_plane_epoch bigint NOT NULL CHECK(control_plane_epoch>0),
 body bytea NOT NULL,sha256 bytea NOT NULL CHECK(octet_length(sha256)=32),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,environment_id,service_key,bundle_hash,revision)
);
