-- +goose Up
ALTER TABLE relay_nodes ADD COLUMN probe_checked_at timestamptz,
    ADD COLUMN probe_succeeded_at timestamptz,
    ADD COLUMN probe_config_version bigint CHECK (probe_config_version > 0),
    ADD COLUMN probe_active_sessions integer CHECK (probe_active_sessions BETWEEN 0 AND 100000),
    ADD COLUMN probe_failures integer NOT NULL DEFAULT 0 CHECK (probe_failures BETWEEN 0 AND 3),
    ADD CONSTRAINT relay_probe_result CHECK (
        (probe_succeeded_at IS NULL)=(probe_config_version IS NULL)
        AND (probe_succeeded_at IS NULL)=(probe_active_sessions IS NULL));

-- +goose Down
ALTER TABLE relay_nodes DROP CONSTRAINT relay_probe_result,
    DROP COLUMN probe_checked_at, DROP COLUMN probe_succeeded_at, DROP COLUMN probe_config_version,
    DROP COLUMN probe_active_sessions, DROP COLUMN probe_failures;
