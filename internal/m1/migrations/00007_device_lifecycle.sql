-- +goose Up
ALTER TABLE devices ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0);
ALTER TABLE device_key_history ADD CONSTRAINT device_key_history_unique_key UNIQUE (public_key);

CREATE TABLE device_key_rotations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id uuid NOT NULL REFERENCES devices(id),
    issued_by uuid NOT NULL REFERENCES users(id),
    device_version bigint NOT NULL,
    digest bytea NOT NULL UNIQUE CHECK (octet_length(digest)=32),
    nonce text NOT NULL,
    restore boolean NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    completed_key bytea CHECK (octet_length(completed_key)=32),
    completed_version bigint,
    CHECK ((completed_key IS NULL)=(completed_version IS NULL))
);
CREATE UNIQUE INDEX device_key_rotation_outstanding ON device_key_rotations(device_id)
    WHERE revoked_at IS NULL AND completed_key IS NULL;

-- +goose Down
DROP TABLE device_key_rotations;
ALTER TABLE device_key_history DROP CONSTRAINT device_key_history_unique_key;
ALTER TABLE devices DROP COLUMN version;
