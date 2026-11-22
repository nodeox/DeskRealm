-- +goose Up
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('pending', 'active', 'disabled', 'archived'));

CREATE TABLE identity_grants (
    token_digest bytea PRIMARY KEY CHECK (octet_length(token_digest) = 32),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    user_id uuid NOT NULL,
    created_by uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('invitation', 'recovery')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    FOREIGN KEY (organization_id, user_id) REFERENCES users(organization_id, id),
    FOREIGN KEY (organization_id, created_by) REFERENCES users(organization_id, id)
);
CREATE UNIQUE INDEX identity_grants_current ON identity_grants(user_id, kind) WHERE consumed_at IS NULL;

CREATE TABLE identity_imports (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    idempotency_key text NOT NULL,
    payload_digest bytea NOT NULL CHECK (octet_length(payload_digest) = 32),
    result jsonb NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, idempotency_key),
    FOREIGN KEY (organization_id, created_by) REFERENCES users(organization_id, id)
);

-- +goose Down
DROP TABLE identity_imports, identity_grants;
UPDATE users SET status = 'disabled' WHERE status = 'pending';
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('active', 'disabled', 'archived'));
