-- +goose Up
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check CHECK
    (kind IN ('user_import','device_enrollment','center_install','delivery','relay_enrollment','relay_lifecycle','windows_delivery','windows_rollout'));
ALTER TABLE devices ADD COLUMN managed_bundle_sha text CHECK (managed_bundle_sha ~ '^[a-f0-9]{64}$');
ALTER TABLE windows_releases ADD COLUMN rollout_only boolean NOT NULL DEFAULT false;

CREATE TABLE windows_rollouts (
    task_id uuid PRIMARY KEY REFERENCES tasks(id),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    release_id uuid NOT NULL,
    rollback_release_id uuid,
    group_id uuid NOT NULL,
    request_id uuid NOT NULL,
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    batch integer NOT NULL DEFAULT 1 CHECK (batch>=1),
    batches integer NOT NULL CHECK (batches>=batch),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision>=1),
    opened_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    canceled_at timestamptz,
    FOREIGN KEY (organization_id,release_id) REFERENCES windows_releases(organization_id,id),
    FOREIGN KEY (organization_id,rollback_release_id) REFERENCES windows_releases(organization_id,id),
    CHECK (rollback_release_id IS DISTINCT FROM release_id),
    FOREIGN KEY (organization_id,group_id) REFERENCES device_groups(organization_id,id),
    UNIQUE (organization_id,request_id)
);

CREATE TABLE windows_rollout_devices (
    task_id uuid NOT NULL REFERENCES windows_rollouts(task_id),
    target_id uuid PRIMARY KEY REFERENCES task_targets(id),
    device_id uuid NOT NULL REFERENCES devices(id),
    batch integer NOT NULL CHECK (batch>=1),
    platform text NOT NULL CHECK (platform IN ('windows-amd64','windows-arm64')),
    key_epoch bigint NOT NULL CHECK (key_epoch>=1),
    installer_sha text NOT NULL CHECK (installer_sha ~ '^[a-f0-9]{64}$'),
    bundle_sha text NOT NULL CHECK (bundle_sha ~ '^[a-f0-9]{64}$'),
    baseline_sha text CHECK (baseline_sha ~ '^[a-f0-9]{64}$'),
    started_at timestamptz,
    held boolean NOT NULL DEFAULT true,
    UNIQUE (task_id,device_id)
);
CREATE UNIQUE INDEX windows_rollout_device_held ON windows_rollout_devices(device_id) WHERE held;

-- +goose Down
DELETE FROM task_attempts WHERE target_id IN (SELECT target_id FROM windows_rollout_devices);
DROP TABLE windows_rollout_devices,windows_rollouts;
DELETE FROM task_targets WHERE task_id IN (SELECT id FROM tasks WHERE kind='windows_rollout');
UPDATE audit_events SET task_id=NULL WHERE task_id IN (SELECT id FROM tasks WHERE kind='windows_rollout');
DELETE FROM tasks WHERE kind='windows_rollout';
ALTER TABLE devices DROP COLUMN managed_bundle_sha;
ALTER TABLE windows_releases DROP COLUMN rollout_only;
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check CHECK
    (kind IN ('user_import','device_enrollment','center_install','delivery','relay_enrollment','relay_lifecycle','windows_delivery'));
