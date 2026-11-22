-- +goose Up
CREATE TABLE bootstrap_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    ticket_digest bytea CHECK (octet_length(ticket_digest) = 32),
    ticket_expires_at timestamptz,
    initialized_at timestamptz,
    CHECK ((ticket_digest IS NULL) = (ticket_expires_at IS NULL)),
    CHECK (initialized_at IS NULL OR ticket_digest IS NULL)
);
INSERT INTO bootstrap_state DEFAULT VALUES;

CREATE TABLE organizations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    singleton boolean NOT NULL DEFAULT true UNIQUE CHECK (singleton),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    username text NOT NULL CHECK (username ~ '^[a-z][a-z0-9._-]{2,39}$'),
    password_hash text,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'archived')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, username),
    UNIQUE (organization_id, id)
);
CREATE TABLE roles (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    name text NOT NULL,
    UNIQUE (organization_id, name),
    UNIQUE (organization_id, id)
);
CREATE TABLE role_bindings (
    organization_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role_id uuid NOT NULL,
    FOREIGN KEY (organization_id, user_id) REFERENCES users(organization_id, id),
    FOREIGN KEY (organization_id, role_id) REFERENCES roles(organization_id, id),
    PRIMARY KEY (user_id, role_id)
);
CREATE TABLE browser_sessions (
    token_digest bytea PRIMARY KEY CHECK (octet_length(token_digest) = 32),
    user_id uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX browser_sessions_user ON browser_sessions(user_id);

CREATE TABLE audit_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    actor_id uuid REFERENCES users(id),
    action text NOT NULL,
    result text NOT NULL,
    resource_id uuid,
    session_id uuid,
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_organization_time ON audit_events(organization_id, occurred_at DESC);
CREATE TABLE outbox (
    event_id uuid PRIMARY KEY REFERENCES audit_events(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz
);

-- +goose Down
DROP TABLE outbox, audit_events, browser_sessions, role_bindings, roles, users, organizations, bootstrap_state;
