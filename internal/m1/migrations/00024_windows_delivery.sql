-- +goose Up
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check
    CHECK (kind IN ('user_import','device_enrollment','center_install','delivery','relay_enrollment','relay_lifecycle','windows_delivery'));

CREATE TABLE windows_delivery_requests (
    task_id uuid PRIMARY KEY REFERENCES tasks(id),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    request_key text NOT NULL,
    manifest_sha256 text NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    manifest jsonb NOT NULL CHECK (jsonb_typeof(manifest)='object'),
    verified_at timestamptz,
    verifier_key_sha256 text CHECK (verifier_key_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK ((verified_at IS NULL) = (verifier_key_sha256 IS NULL)),
    UNIQUE (organization_id,request_key)
);
CREATE TABLE windows_delivery_brand_revisions (
    organization_id uuid NOT NULL REFERENCES organizations(id),
    template_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    brand jsonb NOT NULL CHECK (jsonb_typeof(brand)='object'),
    PRIMARY KEY (organization_id,template_id,revision)
);

-- +goose Down
ALTER TABLE tasks ADD CONSTRAINT tasks_previous_kind_check
    CHECK (kind IN ('user_import','device_enrollment','center_install','delivery','relay_enrollment','relay_lifecycle'));
DROP TABLE windows_delivery_requests,windows_delivery_brand_revisions;
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks RENAME CONSTRAINT tasks_previous_kind_check TO tasks_kind_check;
