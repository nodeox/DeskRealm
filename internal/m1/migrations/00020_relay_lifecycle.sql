-- +goose Up
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check
    CHECK (kind IN ('user_import','device_enrollment','center_install','delivery','relay_enrollment','relay_lifecycle'));

ALTER TABLE relay_nodes ADD COLUMN mode text NOT NULL DEFAULT 'enabled'
    CHECK (mode IN ('enabled','draining','disabled')),
    ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    ADD COLUMN health_config_version bigint CHECK (health_config_version > 0),
    ADD COLUMN last_operation_task_id uuid REFERENCES tasks(id);
UPDATE relay_nodes SET health_config_version=config_version WHERE health_at IS NOT NULL;
ALTER TABLE relay_nodes ADD CHECK ((health_config_version IS NULL)=(health_at IS NULL));

CREATE TABLE relay_probe_history (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES relay_nodes(id),
    endpoint text NOT NULL,
    config_version bigint NOT NULL CHECK (config_version > 0),
    checked_at timestamptz NOT NULL,
    result text NOT NULL CHECK (result IN ('verified','unreachable','protocol_mismatch','invalid_capacity')),
    task_id uuid REFERENCES tasks(id)
);
CREATE INDEX relay_probe_history_node ON relay_probe_history(node_id,checked_at DESC,id DESC);

ALTER TABLE managed_session_routes DROP CONSTRAINT managed_session_routes_relay_id_check;
ALTER TABLE managed_session_routes ADD CONSTRAINT managed_session_routes_relay_id_check
    CHECK (relay_id ~ '^[a-z0-9][a-z0-9_-]{0,31}$'
        OR relay_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');

-- +goose Down
ALTER TABLE managed_session_routes DROP CONSTRAINT managed_session_routes_relay_id_check;
ALTER TABLE managed_session_routes ADD CONSTRAINT managed_session_routes_relay_id_check
    CHECK (relay_id ~ '^[a-z0-9][a-z0-9_-]{0,31}$');
ALTER TABLE tasks ADD CONSTRAINT tasks_previous_kind_check
    CHECK (kind IN ('user_import','device_enrollment','center_install','delivery','relay_enrollment'));
DROP TABLE relay_probe_history;
ALTER TABLE relay_nodes DROP COLUMN last_operation_task_id, DROP COLUMN health_config_version,
    DROP COLUMN revision, DROP COLUMN mode;
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks RENAME CONSTRAINT tasks_previous_kind_check TO tasks_kind_check;
