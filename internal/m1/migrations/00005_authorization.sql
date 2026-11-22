-- +goose Up
CREATE TABLE user_groups (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, id),
    UNIQUE (organization_id, name)
);
CREATE TABLE user_group_members (
    organization_id uuid NOT NULL,
    group_id uuid NOT NULL,
    user_id uuid NOT NULL,
    PRIMARY KEY (group_id, user_id),
    FOREIGN KEY (organization_id, group_id) REFERENCES user_groups(organization_id, id),
    FOREIGN KEY (organization_id, user_id) REFERENCES users(organization_id, id)
);
CREATE INDEX user_group_members_user ON user_group_members(user_id, group_id);

CREATE TABLE management_roles (
    name text PRIMARY KEY
);
INSERT INTO management_roles VALUES ('user_viewer'), ('user_manager');
CREATE TABLE management_role_actions (
    role text NOT NULL REFERENCES management_roles(name),
    action text NOT NULL,
    PRIMARY KEY (role, action)
);
INSERT INTO management_role_actions VALUES
    ('user_viewer','users.read'),
    ('user_manager','users.read'),
    ('user_manager','users.state');

CREATE TABLE management_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    user_id uuid NOT NULL,
    role text NOT NULL REFERENCES management_roles(name),
    scope text NOT NULL CHECK (scope IN ('organization', 'group', 'self')),
    group_id uuid,
    CHECK ((scope = 'group') = (group_id IS NOT NULL)),
    UNIQUE NULLS NOT DISTINCT (user_id, role, scope, group_id),
    FOREIGN KEY (organization_id, user_id) REFERENCES users(organization_id, id),
    FOREIGN KEY (organization_id, group_id) REFERENCES user_groups(organization_id, id)
);
CREATE INDEX management_grants_user ON management_grants(user_id);

CREATE TABLE api_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    digest bytea NOT NULL UNIQUE CHECK (octet_length(digest) = 32),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    user_id uuid NOT NULL,
    purpose text NOT NULL CHECK (char_length(purpose) BETWEEN 1 AND 80),
    action text NOT NULL CHECK (action = 'users.read'),
    scope text NOT NULL CHECK (scope IN ('organization','group','self')),
    group_id uuid,
    CHECK ((scope='group')=(group_id IS NOT NULL)),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id,user_id) REFERENCES users(organization_id,id),
    FOREIGN KEY (organization_id,group_id) REFERENCES user_groups(organization_id,id)
);
CREATE INDEX api_tokens_owner ON api_tokens(user_id,created_at DESC);

CREATE TABLE control_roles (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    allowed_actions text[] NOT NULL CHECK (
        cardinality(allowed_actions) BETWEEN 1 AND 5 AND
        allowed_actions <@ ARRAY['view','input','audio','clipboard','file_transfer']::text[]
    ),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    UNIQUE (organization_id,name),
    UNIQUE (organization_id,id)
);

-- +goose Down
DROP TABLE control_roles, api_tokens, management_grants, management_role_actions, management_roles, user_group_members, user_groups;
