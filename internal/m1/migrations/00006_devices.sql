-- +goose Up
CREATE TABLE device_groups (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    UNIQUE (organization_id, id),
    UNIQUE (organization_id, name)
);
CREATE TABLE device_group_grants (
    organization_id uuid NOT NULL,
    group_id uuid NOT NULL,
    user_id uuid NOT NULL,
    can_approve boolean NOT NULL,
    PRIMARY KEY (group_id,user_id),
    FOREIGN KEY (organization_id,group_id) REFERENCES device_groups(organization_id,id),
    FOREIGN KEY (organization_id,user_id) REFERENCES users(organization_id,id)
);
CREATE TABLE devices (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    group_id uuid NOT NULL,
    platform text NOT NULL CHECK (platform IN ('windows-amd64','windows-arm64')),
    rustdesk_id text NOT NULL,
    public_key bytea NOT NULL CHECK (octet_length(public_key)=32),
    registration_state text NOT NULL DEFAULT 'pending'
        CHECK (registration_state IN ('pending','approved','rejected','disabled','retired')),
    connection_state text NOT NULL DEFAULT 'unknown'
        CHECK (connection_state IN ('unknown','online','offline')),
    expected_policy_version bigint,
    applied_policy_version bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id,group_id) REFERENCES device_groups(organization_id,id),
    UNIQUE (organization_id,id),
    UNIQUE (organization_id,public_key)
);
CREATE UNIQUE INDEX devices_live_rustdesk_id ON devices(organization_id,rustdesk_id)
    WHERE registration_state <> 'retired';
CREATE INDEX devices_group ON devices(group_id,id);
CREATE TABLE device_key_history (
    device_id uuid NOT NULL REFERENCES devices(id),
    public_key bytea NOT NULL CHECK (octet_length(public_key)=32),
    recorded_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    PRIMARY KEY (device_id,public_key)
);
CREATE TABLE rustdesk_id_history (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    device_id uuid NOT NULL REFERENCES devices(id),
    rustdesk_id text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE enrollment_credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    group_id uuid NOT NULL,
    platform text NOT NULL CHECK (platform IN ('windows-amd64','windows-arm64')),
    purpose text NOT NULL CHECK (char_length(purpose) BETWEEN 1 AND 80),
    digest bytea UNIQUE NOT NULL CHECK (octet_length(digest)=32),
    issued_by uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    FOREIGN KEY (organization_id,group_id) REFERENCES device_groups(organization_id,id),
    FOREIGN KEY (organization_id,issued_by) REFERENCES users(organization_id,id)
);
CREATE TABLE enrollment_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    credential_id uuid NOT NULL UNIQUE REFERENCES enrollment_credentials(id),
    nonce text NOT NULL,
    group_id uuid NOT NULL REFERENCES device_groups(id),
    platform text NOT NULL,
    rustdesk_id text NOT NULL,
    public_key bytea NOT NULL CHECK (octet_length(public_key)=32),
    expires_at timestamptz NOT NULL,
    device_id uuid REFERENCES devices(id)
);
CREATE TABLE registration_approvals (
    device_id uuid PRIMARY KEY REFERENCES devices(id),
    decision text CHECK (decision IN ('approved','rejected')),
    decided_by uuid REFERENCES users(id),
    decided_at timestamptz,
    CHECK ((decision IS NULL)=(decided_by IS NULL)),
    CHECK ((decision IS NULL)=(decided_at IS NULL))
);

-- +goose Down
DROP TABLE registration_approvals,enrollment_attempts,enrollment_credentials,rustdesk_id_history,
    device_key_history,devices,device_group_grants,device_groups;
