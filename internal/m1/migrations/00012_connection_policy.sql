-- +goose Up
ALTER TABLE devices ADD COLUMN managed_heartbeat_protocol integer
    CHECK (managed_heartbeat_protocol BETWEEN 1 AND 65535);
-- Migration 11 accepted only managed protocol 1.
UPDATE devices SET managed_heartbeat_protocol=1 WHERE managed_heartbeat_at IS NOT NULL;
ALTER TABLE devices ADD CONSTRAINT devices_managed_heartbeat_protocol_complete
    CHECK ((managed_heartbeat_at IS NULL) = (managed_heartbeat_protocol IS NULL));

CREATE TABLE connection_rules (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    latest_version bigint NOT NULL DEFAULT 1 CHECK (latest_version > 0),
    published_version bigint CHECK (published_version > 0 AND published_version <= latest_version),
    archived_at timestamptz,
    UNIQUE (organization_id,id)
);
CREATE INDEX connection_rules_live ON connection_rules(organization_id,id) WHERE archived_at IS NULL;

CREATE TABLE connection_rule_versions (
    organization_id uuid NOT NULL,
    rule_id uuid NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    source_user_id uuid,
    source_group_id uuid,
    controller_device_id uuid,
    target_device_id uuid,
    target_group_id uuid,
    purpose text NOT NULL CHECK (purpose ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    effect text NOT NULL CHECK (effect IN ('allow','deny')),
    role_id uuid,
    role_version bigint CHECK (role_version > 0),
    allowed_actions text[] NOT NULL DEFAULT '{}',
    created_by uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (rule_id,version),
    UNIQUE (organization_id,rule_id,version),
    CHECK ((source_user_id IS NULL) <> (source_group_id IS NULL)),
    CHECK ((target_device_id IS NULL) <> (target_group_id IS NULL)),
    CHECK ((effect='allow' AND role_id IS NOT NULL AND role_version IS NOT NULL
        AND cardinality(allowed_actions) BETWEEN 1 AND 5) OR
        (effect='deny' AND role_id IS NULL AND role_version IS NULL AND cardinality(allowed_actions)=0)),
    CHECK (allowed_actions <@ ARRAY['view','input','audio','clipboard','file_transfer']::text[]),
    FOREIGN KEY (organization_id,rule_id) REFERENCES connection_rules(organization_id,id),
    FOREIGN KEY (organization_id,source_user_id) REFERENCES users(organization_id,id),
    FOREIGN KEY (organization_id,source_group_id) REFERENCES user_groups(organization_id,id),
    FOREIGN KEY (organization_id,controller_device_id) REFERENCES devices(organization_id,id),
    FOREIGN KEY (organization_id,target_device_id) REFERENCES devices(organization_id,id),
    FOREIGN KEY (organization_id,target_group_id) REFERENCES device_groups(organization_id,id),
    FOREIGN KEY (organization_id,role_id) REFERENCES control_roles(organization_id,id),
    FOREIGN KEY (organization_id,created_by) REFERENCES users(organization_id,id)
);
CREATE INDEX connection_rule_versions_by_purpose ON connection_rule_versions(organization_id,purpose,rule_id,version);
ALTER TABLE connection_rules
    ADD CONSTRAINT connection_rules_latest_revision FOREIGN KEY (organization_id,id,latest_version)
        REFERENCES connection_rule_versions(organization_id,rule_id,version) DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT connection_rules_published_revision FOREIGN KEY (organization_id,id,published_version)
        REFERENCES connection_rule_versions(organization_id,rule_id,version);

-- +goose Down
ALTER TABLE connection_rules DROP CONSTRAINT connection_rules_latest_revision,
    DROP CONSTRAINT connection_rules_published_revision;
DROP TABLE connection_rule_versions,connection_rules;
ALTER TABLE devices DROP CONSTRAINT devices_managed_heartbeat_protocol_complete,
    DROP COLUMN managed_heartbeat_protocol;
