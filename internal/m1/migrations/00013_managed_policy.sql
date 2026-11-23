-- +goose Up
ALTER TABLE devices ADD COLUMN managed_policy_owner_id uuid,
    ADD CONSTRAINT managed_policy_device_owner FOREIGN KEY (organization_id,managed_policy_owner_id)
        REFERENCES users(organization_id,id);

CREATE TABLE managed_policy_versions (
    version bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    schema_version smallint NOT NULL CHECK (schema_version=1),
    allowed_actions text[] NOT NULL CHECK (cardinality(allowed_actions)<=5 AND
        allowed_actions <@ ARRAY['view','input','audio','clipboard','file_transfer']::text[]),
    unattended_purposes text[] NOT NULL CHECK (cardinality(unattended_purposes)<=32),
    payload_sha256 bytea NOT NULL CHECK (octet_length(payload_sha256)=32),
    published_by uuid NOT NULL,
    published_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (organization_id,version),
    FOREIGN KEY (organization_id,published_by) REFERENCES users(organization_id,id)
);

CREATE TABLE managed_policy_assignments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    scope text NOT NULL CHECK (scope IN ('device','user','group','organization')),
    device_id uuid,
    user_id uuid,
    group_id uuid,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
    CHECK ((scope='device')=(device_id IS NOT NULL) AND (scope='user')=(user_id IS NOT NULL)
        AND (scope='group')=(group_id IS NOT NULL)),
    UNIQUE NULLS NOT DISTINCT (organization_id,scope,device_id,user_id,group_id),
    UNIQUE (organization_id,id),
    FOREIGN KEY (organization_id,device_id) REFERENCES devices(organization_id,id),
    FOREIGN KEY (organization_id,user_id) REFERENCES users(organization_id,id),
    FOREIGN KEY (organization_id,group_id) REFERENCES device_groups(organization_id,id)
);

CREATE TABLE managed_policy_assignment_revisions (
    organization_id uuid NOT NULL,
    assignment_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision>0),
    policy_version bigint,
    canary_version bigint,
    canary_percent smallint NOT NULL DEFAULT 0 CHECK (canary_percent BETWEEN 0 AND 99),
    rollback_from bigint,
    rollback_to bigint,
    changed_by uuid NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (assignment_id,revision),
    UNIQUE (organization_id,assignment_id,revision),
    CHECK ((canary_version IS NULL)=(canary_percent=0) AND
        (canary_version IS NULL OR canary_version IS DISTINCT FROM policy_version)),
    CHECK ((rollback_from IS NULL)=(rollback_to IS NULL) AND
        (rollback_from IS NULL OR (rollback_from=revision-1 AND rollback_to<rollback_from))),
    FOREIGN KEY (organization_id,assignment_id) REFERENCES managed_policy_assignments(organization_id,id),
    FOREIGN KEY (organization_id,policy_version) REFERENCES managed_policy_versions(organization_id,version),
    FOREIGN KEY (organization_id,canary_version) REFERENCES managed_policy_versions(organization_id,version),
    FOREIGN KEY (assignment_id,rollback_from) REFERENCES managed_policy_assignment_revisions(assignment_id,revision),
    FOREIGN KEY (assignment_id,rollback_to) REFERENCES managed_policy_assignment_revisions(assignment_id,revision),
    FOREIGN KEY (organization_id,changed_by) REFERENCES users(organization_id,id)
);
ALTER TABLE managed_policy_assignments ADD CONSTRAINT managed_policy_assignment_head
    FOREIGN KEY (organization_id,id,revision)
    REFERENCES managed_policy_assignment_revisions(organization_id,assignment_id,revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE managed_policy_deliveries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    device_id uuid NOT NULL,
    policy_version bigint NOT NULL,
    key_epoch bigint NOT NULL CHECK (key_epoch>0),
    assignment_id uuid NOT NULL,
    assignment_revision bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (id,device_id),
    FOREIGN KEY (organization_id,device_id) REFERENCES devices(organization_id,id),
    FOREIGN KEY (organization_id,policy_version) REFERENCES managed_policy_versions(organization_id,version),
    FOREIGN KEY (organization_id,assignment_id,assignment_revision)
        REFERENCES managed_policy_assignment_revisions(organization_id,assignment_id,revision)
);
ALTER TABLE devices ADD COLUMN managed_policy_delivery_id uuid,
    ADD CONSTRAINT managed_policy_device_delivery FOREIGN KEY (managed_policy_delivery_id,id)
        REFERENCES managed_policy_deliveries(id,device_id);

CREATE TABLE managed_policy_receipts (
    delivery_id uuid PRIMARY KEY REFERENCES managed_policy_deliveries(id),
    result text NOT NULL CHECK (result IN ('applied','failed')),
    failed_mask smallint NOT NULL CHECK (failed_mask BETWEEN 0 AND 7 AND (failed_mask=0)=(result='applied')),
    signature bytea NOT NULL CHECK (octet_length(signature)=64),
    signer_key bytea NOT NULL CHECK (octet_length(signer_key)=32),
    received_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- +goose Down
UPDATE devices SET expected_policy_version=NULL WHERE managed_policy_delivery_id IS NOT NULL;
UPDATE devices d SET applied_policy_version=NULL WHERE EXISTS (
    SELECT 1 FROM managed_policy_deliveries p JOIN managed_policy_receipts r ON r.delivery_id=p.id
    WHERE p.device_id=d.id AND p.policy_version=d.applied_policy_version AND r.result='applied'
);
ALTER TABLE devices DROP CONSTRAINT managed_policy_device_delivery, DROP CONSTRAINT managed_policy_device_owner,
    DROP COLUMN managed_policy_delivery_id, DROP COLUMN managed_policy_owner_id;
ALTER TABLE managed_policy_assignments DROP CONSTRAINT managed_policy_assignment_head;
DROP TABLE managed_policy_receipts,managed_policy_deliveries,managed_policy_assignment_revisions,
    managed_policy_assignments,managed_policy_versions;
