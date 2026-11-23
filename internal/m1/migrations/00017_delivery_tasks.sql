-- +goose Up
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check CHECK (kind IN ('user_import','device_enrollment','center_install','delivery'));
ALTER TABLE task_targets DROP CONSTRAINT task_targets_state_check, DROP CONSTRAINT task_targets_attempt_count_check;
ALTER TABLE task_targets
    ADD CONSTRAINT task_targets_state_check CHECK (state IN ('waiting','queued','running','succeeded','failed','canceled')),
    ADD CONSTRAINT task_targets_attempt_count_check CHECK (attempt_count BETWEEN 0 AND 6),
    ADD COLUMN operation_id uuid UNIQUE;
ALTER TABLE task_attempts DROP CONSTRAINT task_attempts_sequence_check;
ALTER TABLE task_attempts ADD CONSTRAINT task_attempts_sequence_check CHECK (sequence BETWEEN 0 AND 6);

CREATE TABLE delivery_task_requests (
    task_id uuid PRIMARY KEY REFERENCES tasks(id),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    request_key text NOT NULL,
    payload_digest bytea NOT NULL CHECK (octet_length(payload_digest)=32),
    UNIQUE (organization_id,request_key)
);

CREATE TABLE delivery_task_receipts (
    receipt_id uuid PRIMARY KEY,
    attempt_id uuid NOT NULL UNIQUE REFERENCES task_attempts(id),
    operation_id uuid NOT NULL REFERENCES task_targets(operation_id),
    succeeded boolean NOT NULL
);

-- +goose Down
ALTER TABLE tasks ADD CONSTRAINT tasks_previous_kind_check CHECK (kind IN ('user_import','device_enrollment','center_install'));
DROP TABLE delivery_task_receipts,delivery_task_requests;
ALTER TABLE task_attempts DROP CONSTRAINT task_attempts_sequence_check;
ALTER TABLE task_attempts ADD CONSTRAINT task_attempts_sequence_check CHECK (sequence BETWEEN 0 AND 5);
ALTER TABLE task_targets DROP COLUMN operation_id, DROP CONSTRAINT task_targets_state_check, DROP CONSTRAINT task_targets_attempt_count_check;
ALTER TABLE task_targets
    ADD CONSTRAINT task_targets_state_check CHECK (state IN ('waiting','queued','running','succeeded','failed')),
    ADD CONSTRAINT task_targets_attempt_count_check CHECK (attempt_count BETWEEN 0 AND 5);
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks RENAME CONSTRAINT tasks_previous_kind_check TO tasks_kind_check;
