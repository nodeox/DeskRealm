-- +goose Up
ALTER TABLE enrollment_attempts ADD COLUMN receipt_state text NOT NULL DEFAULT 'pending'
    CHECK (receipt_state IN ('pending','approved'));

CREATE TABLE windows_enrollment_bindings (
    credential_id uuid PRIMARY KEY REFERENCES enrollment_credentials(id),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    deployment_id uuid NOT NULL,
    release_id uuid NOT NULL,
    group_id uuid NOT NULL,
    public_key bytea NOT NULL CHECK (octet_length(public_key)=32),
    rustdesk_id text NOT NULL,
    auto_approve boolean NOT NULL,
    lifetime_minutes integer NOT NULL CHECK (lifetime_minutes BETWEEN 1 AND 1440),
    revoked_at timestamptz,
    UNIQUE (organization_id,deployment_id),
    FOREIGN KEY (organization_id,release_id) REFERENCES windows_releases(organization_id,id),
    FOREIGN KEY (organization_id,group_id) REFERENCES device_groups(organization_id,id),
    FOREIGN KEY (release_id,group_id) REFERENCES windows_release_groups(release_id,group_id)
);

-- +goose Down
-- Do not remove the device and release restrictions from existing credentials.
UPDATE enrollment_credentials e SET expires_at=LEAST(expires_at,clock_timestamp())
    FROM windows_enrollment_bindings b WHERE b.credential_id=e.id;
DELETE FROM enrollment_attempts a USING windows_enrollment_bindings b
    WHERE a.credential_id=b.credential_id AND a.device_id IS NULL;
DROP TABLE windows_enrollment_bindings;
ALTER TABLE enrollment_attempts DROP COLUMN receipt_state;
