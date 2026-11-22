-- +goose Up
ALTER TABLE devices
    ADD COLUMN device_key_epoch bigint NOT NULL DEFAULT 1 CHECK (device_key_epoch > 0),
    ADD COLUMN managed_heartbeat_at timestamptz,
    ADD COLUMN managed_heartbeat_epoch bigint,
    ADD COLUMN managed_reported_build text,
    ADD CONSTRAINT devices_managed_heartbeat_complete CHECK (
        (managed_heartbeat_at IS NULL) = (managed_heartbeat_epoch IS NULL)
        AND (managed_heartbeat_at IS NULL) = (managed_reported_build IS NULL)
    );
UPDATE devices SET connection_state='unknown' WHERE connection_state='online';

CREATE TABLE managed_device_credentials (
    digest bytea PRIMARY KEY CHECK (octet_length(digest)=32),
    device_id uuid NOT NULL REFERENCES devices(id),
    key_epoch bigint NOT NULL CHECK (key_epoch > 0),
    protocol_version integer NOT NULL CHECK (protocol_version BETWEEN 1 AND 65535),
    expires_at timestamptz NOT NULL,
    UNIQUE(digest,device_id)
);
CREATE INDEX managed_device_credentials_by_device ON managed_device_credentials(device_id,expires_at);

CREATE TABLE managed_identity_challenges (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id uuid NOT NULL REFERENCES devices(id),
    key_epoch bigint NOT NULL CHECK (key_epoch > 0),
    protocol_version integer NOT NULL CHECK (protocol_version BETWEEN 1 AND 65535),
    nonce text NOT NULL CHECK (char_length(nonce)=43),
    device_digest bytea,
    browser_digest bytea REFERENCES browser_sessions(token_digest) ON DELETE CASCADE,
    flow text CHECK (flow IS NULL OR char_length(flow)=43),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    CHECK ((device_digest IS NULL)=(browser_digest IS NULL) AND (device_digest IS NULL)=(flow IS NULL)),
    FOREIGN KEY (device_digest,device_id) REFERENCES managed_device_credentials(digest,device_id) ON DELETE CASCADE,
    UNIQUE NULLS NOT DISTINCT (device_id,device_digest,browser_digest)
);

CREATE TABLE managed_controller_credentials (
    digest bytea PRIMARY KEY CHECK (octet_length(digest)=32),
    device_digest bytea NOT NULL REFERENCES managed_device_credentials(digest) ON DELETE CASCADE,
    browser_digest bytea NOT NULL REFERENCES browser_sessions(token_digest) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE INDEX managed_controller_credentials_binding ON managed_controller_credentials(device_digest,browser_digest,expires_at);

-- +goose Down
DROP TABLE managed_controller_credentials,managed_identity_challenges,managed_device_credentials;
ALTER TABLE devices DROP CONSTRAINT devices_managed_heartbeat_complete,
    DROP COLUMN device_key_epoch, DROP COLUMN managed_heartbeat_at,
    DROP COLUMN managed_heartbeat_epoch, DROP COLUMN managed_reported_build;
