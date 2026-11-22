-- +goose Up
CREATE TABLE user_totp (
    user_id uuid PRIMARY KEY REFERENCES users(id),
    ciphertext bytea NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    last_step bigint NOT NULL DEFAULT -1
);
CREATE TABLE totp_challenges (
    digest bytea PRIMARY KEY CHECK (octet_length(digest) = 32),
    user_id uuid NOT NULL UNIQUE REFERENCES users(id),
    expires_at timestamptz NOT NULL,
    failures integer NOT NULL DEFAULT 0
);
CREATE TABLE oidc_identities (
    issuer text NOT NULL,
    subject text NOT NULL,
    user_id uuid NOT NULL REFERENCES users(id),
    PRIMARY KEY (issuer, subject),
    UNIQUE (issuer, user_id)
);
CREATE TABLE oidc_logins (
    digest bytea PRIMARY KEY CHECK (octet_length(digest) = 32),
    browser_digest bytea NOT NULL CHECK (octet_length(browser_digest) = 32),
    verifier text NOT NULL,
    nonce text NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX oidc_logins_expiry ON oidc_logins(expires_at);

-- +goose Down
DROP TABLE oidc_logins, oidc_identities, totp_challenges, user_totp;
