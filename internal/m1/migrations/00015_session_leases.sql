-- +goose Up
ALTER TABLE managed_sessions ADD COLUMN browser_digest bytea CHECK (octet_length(browser_digest)=32);
UPDATE managed_sessions g SET browser_digest=c.browser_digest
FROM managed_controller_credentials c WHERE c.digest=g.controller_digest;

ALTER TABLE managed_session_redemptions
    ADD COLUMN receipt_digest bytea CHECK (octet_length(receipt_digest)=32),
    ADD COLUMN lease_sequence bigint NOT NULL DEFAULT 1 CHECK (lease_sequence>=1),
    ADD COLUMN renewed_at timestamptz,
    ADD COLUMN renewed_until timestamptz,
    ADD CHECK ((lease_sequence=1 AND renewed_at IS NULL AND renewed_until IS NULL)
        OR (lease_sequence>1 AND renewed_at IS NOT NULL AND renewed_until IS NOT NULL
            AND renewed_until=renewed_at+interval '30 seconds'));

CREATE TABLE managed_session_revocations (
    session_id uuid PRIMARY KEY REFERENCES managed_sessions(id),
    requested_at timestamptz NOT NULL DEFAULT date_trunc('milliseconds',clock_timestamp()),
    reason text NOT NULL
);

CREATE TABLE managed_session_stops (
    session_id uuid PRIMARY KEY REFERENCES managed_session_redemptions(session_id),
    lease_sequence bigint NOT NULL CHECK (lease_sequence>=1),
    reason text NOT NULL CHECK (reason IN ('local_end','revoked','lease_expired','connection_lost')),
    received_at timestamptz NOT NULL DEFAULT date_trunc('milliseconds',clock_timestamp())
);

CREATE TABLE managed_session_file_events (
    session_id uuid NOT NULL REFERENCES managed_session_redemptions(session_id),
    event_id uuid NOT NULL,
    lease_sequence bigint NOT NULL CHECK (lease_sequence>=1),
    direction text NOT NULL CHECK (direction IN ('upload','download')),
    result text NOT NULL CHECK (result IN ('completed','failed','cancelled')),
    size_bytes bigint NOT NULL CHECK (size_bytes>=0),
    received_at timestamptz NOT NULL DEFAULT date_trunc('milliseconds',clock_timestamp()),
    PRIMARY KEY (session_id,event_id)
);

CREATE INDEX managed_sessions_user ON managed_sessions(user_id);
CREATE INDEX managed_sessions_target ON managed_sessions(target_device_id);
CREATE INDEX managed_sessions_controller ON managed_sessions(controller_device_id);

-- +goose Down
DROP INDEX managed_sessions_controller,managed_sessions_target,managed_sessions_user;
DROP TABLE managed_session_file_events,managed_session_stops,managed_session_revocations;
ALTER TABLE managed_session_redemptions DROP COLUMN renewed_until, DROP COLUMN renewed_at,
    DROP COLUMN lease_sequence, DROP COLUMN receipt_digest;
ALTER TABLE managed_sessions DROP COLUMN browser_digest;
