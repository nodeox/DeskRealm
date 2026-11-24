-- +goose Up
CREATE TABLE windows_releases (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    task_id uuid NOT NULL REFERENCES windows_delivery_requests(task_id),
    manifest_sha256 text NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    version text NOT NULL,
    signature_scope text NOT NULL CHECK (signature_scope='development'),
    packages jsonb NOT NULL CHECK (jsonb_typeof(packages)='object'),
    verifier_key_sha256 text NOT NULL CHECK (verifier_key_sha256 ~ '^[0-9a-f]{64}$'),
    published_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    UNIQUE (organization_id,id),
    UNIQUE (organization_id,manifest_sha256)
);
CREATE INDEX windows_releases_list ON windows_releases(organization_id,published_at DESC,id DESC);

CREATE TABLE windows_release_groups (
    organization_id uuid NOT NULL,
    release_id uuid NOT NULL,
    group_id uuid NOT NULL,
    PRIMARY KEY (release_id,group_id),
    FOREIGN KEY (organization_id,release_id) REFERENCES windows_releases(organization_id,id),
    FOREIGN KEY (organization_id,group_id) REFERENCES device_groups(organization_id,id)
);

-- +goose Down
DROP TABLE windows_release_groups,windows_releases;
