-- +goose Up
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check
    CHECK (kind IN ('user_import','device_enrollment','center_install','delivery','relay_enrollment'));

CREATE TABLE relay_nodes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    task_id uuid NOT NULL UNIQUE REFERENCES tasks(id),
    name text NOT NULL,
    site text NOT NULL,
    region text NOT NULL,
    endpoint text NOT NULL UNIQUE,
    build text NOT NULL,
    capacity integer NOT NULL CHECK (capacity BETWEEN 1 AND 100000),
    config_version bigint NOT NULL DEFAULT 1 CHECK (config_version > 0),
    identity_key bytea UNIQUE CHECK (octet_length(identity_key)=32),
    registered_at timestamptz,
    health_sequence bigint NOT NULL DEFAULT 0 CHECK (health_sequence >= 0),
    health_at timestamptz,
    active_sessions integer CHECK (active_sessions BETWEEN 0 AND capacity),
    protocol_ready boolean,
    CHECK ((identity_key IS NULL)=(registered_at IS NULL)),
    CHECK ((health_at IS NULL)=(health_sequence=0) AND
        (health_at IS NULL)=(active_sessions IS NULL) AND (health_at IS NULL)=(protocol_ready IS NULL))
);

CREATE TABLE relay_enrollments (
    node_id uuid PRIMARY KEY REFERENCES relay_nodes(id),
    credential_digest bytea NOT NULL UNIQUE CHECK (octet_length(credential_digest)=32),
    expires_at timestamptz NOT NULL,
    challenge text CHECK (length(challenge)=43),
    challenge_expires_at timestamptz,
    challenge_key bytea CHECK (octet_length(challenge_key)=32),
    consumed_at timestamptz,
    CHECK ((challenge IS NULL)=(challenge_key IS NULL) AND (challenge IS NULL)=(challenge_expires_at IS NULL))
);

-- +goose Down
ALTER TABLE tasks ADD CONSTRAINT tasks_previous_kind_check
    CHECK (kind IN ('user_import','device_enrollment','center_install','delivery'));
DROP TABLE relay_enrollments,relay_nodes;
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks RENAME CONSTRAINT tasks_previous_kind_check TO tasks_kind_check;
