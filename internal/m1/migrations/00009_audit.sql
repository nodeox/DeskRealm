-- +goose Up
ALTER TABLE audit_events
    ADD COLUMN source text NOT NULL DEFAULT 'control_plane',
    ADD COLUMN source_key text,
    ADD COLUMN task_id uuid,
    ADD COLUMN device_group_id uuid,
    ADD COLUMN user_group_ids uuid[] NOT NULL DEFAULT '{}',
    ADD COLUMN received_at timestamptz,
    ADD COLUMN receipt_state text NOT NULL DEFAULT 'confirmed'
        CHECK (receipt_state IN ('pending','confirmed')),
    ADD COLUMN receipt_source text,
    ADD COLUMN receipt_key text,
    ADD COLUMN receipt_result text CHECK (receipt_result IN ('success','failure')),
    ADD COLUMN acknowledged_at timestamptz,
    ADD COLUMN duplicate_receipts integer NOT NULL DEFAULT 0 CHECK (duplicate_receipts >= 0),
    ADD CONSTRAINT audit_device_group_scope FOREIGN KEY (organization_id,device_group_id) REFERENCES device_groups(organization_id,id),
    ADD CONSTRAINT audit_receipt_complete CHECK (
        (receipt_source IS NULL AND receipt_state = 'confirmed' AND receipt_key IS NULL AND
          receipt_result IS NULL AND acknowledged_at IS NULL) OR
        (receipt_source IS NOT NULL AND (
            (receipt_state = 'pending' AND receipt_key IS NULL AND receipt_result IS NULL AND acknowledged_at IS NULL) OR
            (receipt_state = 'confirmed' AND receipt_key IS NOT NULL AND receipt_result IS NOT NULL AND acknowledged_at IS NOT NULL)))
    );

UPDATE audit_events SET received_at=occurred_at;
ALTER TABLE audit_events ALTER COLUMN received_at SET DEFAULT clock_timestamp(), ALTER COLUMN received_at SET NOT NULL;

CREATE UNIQUE INDEX audit_events_source_key ON audit_events(organization_id,source,source_key);
CREATE UNIQUE INDEX audit_events_receipt_key ON audit_events(organization_id,receipt_source,receipt_key);
CREATE INDEX audit_events_cursor ON audit_events(organization_id,occurred_at DESC,id DESC);
CREATE INDEX audit_events_session ON audit_events(organization_id,session_id,occurred_at DESC,id DESC) WHERE session_id IS NOT NULL;
CREATE INDEX audit_events_task ON audit_events(organization_id,task_id,occurred_at DESC,id DESC) WHERE task_id IS NOT NULL;

CREATE TABLE audit_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    user_id uuid NOT NULL,
    scope text NOT NULL CHECK (scope IN ('organization','self','user_group','device_group')),
    user_group_id uuid,
    device_group_id uuid,
    can_export boolean NOT NULL DEFAULT false,
    CHECK ((scope='user_group')=(user_group_id IS NOT NULL)),
    CHECK ((scope='device_group')=(device_group_id IS NOT NULL)),
    UNIQUE NULLS NOT DISTINCT (user_id,scope,user_group_id,device_group_id),
    FOREIGN KEY (organization_id,user_id) REFERENCES users(organization_id,id),
    FOREIGN KEY (organization_id,user_group_id) REFERENCES user_groups(organization_id,id),
    FOREIGN KEY (organization_id,device_group_id) REFERENCES device_groups(organization_id,id)
);
CREATE TABLE audit_retention (
    organization_id uuid PRIMARY KEY REFERENCES organizations(id),
    days integer NOT NULL CHECK (days BETWEEN 1 AND 3650)
);

-- +goose Down
DROP TABLE audit_retention,audit_grants;
DROP INDEX audit_events_source_key,audit_events_receipt_key,audit_events_cursor,audit_events_session,audit_events_task;
ALTER TABLE audit_events
    DROP CONSTRAINT audit_receipt_complete, DROP CONSTRAINT audit_device_group_scope, DROP COLUMN source, DROP COLUMN source_key,
    DROP COLUMN task_id, DROP COLUMN device_group_id, DROP COLUMN user_group_ids, DROP COLUMN received_at,
    DROP COLUMN receipt_state, DROP COLUMN receipt_source, DROP COLUMN receipt_key,
    DROP COLUMN receipt_result, DROP COLUMN acknowledged_at, DROP COLUMN duplicate_receipts;
