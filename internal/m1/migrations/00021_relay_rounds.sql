-- +goose Up
CREATE TABLE session_relay_rounds (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES managed_sessions(id),
    connection_id uuid NOT NULL,
    version bigint NOT NULL CHECK (version>0),
    opened_at timestamptz,
    deadline timestamptz,
    superseded_at timestamptz,
    UNIQUE (session_id,connection_id),
    UNIQUE (session_id,version),
    CHECK ((opened_at IS NULL AND deadline IS NULL) OR
           (opened_at IS NOT NULL AND deadline IS NOT NULL AND deadline>opened_at
            AND deadline<=opened_at+interval '2 seconds'))
);
CREATE UNIQUE INDEX session_relay_current_round
    ON session_relay_rounds(session_id) WHERE superseded_at IS NULL;

CREATE TABLE session_relay_candidates (
    round_id uuid NOT NULL REFERENCES session_relay_rounds(id),
    relay_id uuid NOT NULL REFERENCES relay_nodes(id),
    site text NOT NULL,
    region text NOT NULL,
    endpoint text NOT NULL,
    build text NOT NULL,
    identity_key bytea NOT NULL CHECK (octet_length(identity_key)=32),
    config_version bigint NOT NULL CHECK (config_version>0),
    node_revision bigint NOT NULL CHECK (node_revision>0),
    PRIMARY KEY (round_id,relay_id)
);

CREATE TABLE session_relay_reports (
    round_id uuid NOT NULL,
    relay_id uuid NOT NULL,
    role smallint NOT NULL CHECK (role IN (1,2)),
    samples jsonb NOT NULL CHECK (jsonb_typeof(samples)='array' AND jsonb_array_length(samples)=3),
    received_at timestamptz NOT NULL,
    PRIMARY KEY (round_id,relay_id,role),
    FOREIGN KEY (round_id,relay_id) REFERENCES session_relay_candidates(round_id,relay_id)
);

-- +goose Down
DROP TABLE session_relay_reports,session_relay_candidates,session_relay_rounds;
