-- +goose Up
CREATE TABLE managed_sessions (
    id uuid PRIMARY KEY,
    grant_id uuid NOT NULL UNIQUE,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    user_id uuid NOT NULL,
    controller_device_id uuid NOT NULL,
    target_device_id uuid NOT NULL,
    controller_digest bytea NOT NULL CHECK (octet_length(controller_digest)=32),
    request_id uuid NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
    book_id uuid,
    entry_id uuid,
    purpose text NOT NULL CHECK (purpose ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    requested_actions text[] NOT NULL CHECK (cardinality(requested_actions) BETWEEN 1 AND 5 AND
        requested_actions <@ ARRAY['view','input','audio','clipboard','file_transfer']::text[]),
    controller_rustdesk_id text NOT NULL,
    target_rustdesk_id text NOT NULL,
    controller_key bytea NOT NULL CHECK (octet_length(controller_key)=32),
    target_key bytea NOT NULL CHECK (octet_length(target_key)=32),
    policy_version bigint NOT NULL,
    rule_id uuid NOT NULL,
    rule_version bigint NOT NULL,
    allowed_mask smallint NOT NULL CHECK (allowed_mask BETWEEN 1 AND 31),
    confirmation_mode text NOT NULL CHECK (confirmation_mode IN ('attended','unattended')),
    signing_kid text NOT NULL CHECK (signing_kid ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
    signature bytea NOT NULL CHECK (octet_length(signature)=64),
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at>issued_at AND expires_at<=issued_at+interval '60 seconds'),
    CHECK (controller_device_id<>target_device_id AND (book_id IS NULL)=(entry_id IS NULL)),
    UNIQUE (organization_id,user_id,controller_device_id,request_id),
    FOREIGN KEY (organization_id,user_id) REFERENCES users(organization_id,id),
    FOREIGN KEY (organization_id,controller_device_id) REFERENCES devices(organization_id,id),
    FOREIGN KEY (organization_id,target_device_id) REFERENCES devices(organization_id,id),
    FOREIGN KEY (organization_id,policy_version) REFERENCES managed_policy_versions(organization_id,version),
    FOREIGN KEY (organization_id,rule_id,rule_version) REFERENCES connection_rule_versions(organization_id,rule_id,version)
);

CREATE TABLE managed_session_routes (
    session_id uuid NOT NULL REFERENCES managed_sessions(id),
    connection_id uuid NOT NULL,
    path text NOT NULL CHECK (path IN ('direct','relay')),
    relay_id text CHECK (relay_id ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
    authorized_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    paired_at timestamptz,
    CHECK ((path='relay')=(relay_id IS NOT NULL) AND (paired_at IS NULL OR path='relay')),
    PRIMARY KEY (session_id,connection_id)
);

CREATE TABLE managed_session_redemptions (
    session_id uuid PRIMARY KEY,
    connection_id uuid NOT NULL,
    proof_hash bytea NOT NULL CHECK (octet_length(proof_hash)=32),
    allowed_mask smallint NOT NULL CHECK (allowed_mask BETWEEN 1 AND 31),
    path text NOT NULL CHECK (path IN ('direct','relay')),
    relay_id text,
    redeemed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL CHECK (lease_until>redeemed_at AND lease_until<=redeemed_at+interval '30 seconds'),
    CHECK ((path='relay')=(relay_id IS NOT NULL)),
    FOREIGN KEY (session_id,connection_id) REFERENCES managed_session_routes(session_id,connection_id)
);

-- +goose Down
DROP TABLE managed_session_redemptions,managed_session_routes,managed_sessions;
