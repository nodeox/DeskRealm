-- +goose Up
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check CHECK (kind IN ('user_import','device_enrollment','center_install'));

CREATE TABLE installation_task_records (
    task_id uuid NOT NULL REFERENCES tasks(id),
    revision bigint NOT NULL CHECK (revision > 0),
    snapshot jsonb NOT NULL,
    PRIMARY KEY (task_id,revision)
);

-- +goose Down
-- The previous schema cannot represent imported installation tasks.
ALTER TABLE tasks ADD CONSTRAINT tasks_previous_kind_check CHECK (kind IN ('user_import','device_enrollment'));
DROP TABLE installation_task_records;
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks RENAME CONSTRAINT tasks_previous_kind_check TO tasks_kind_check;
