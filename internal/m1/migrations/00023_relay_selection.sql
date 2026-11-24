-- +goose Up
CREATE TABLE session_relay_selections (
    round_id uuid NOT NULL,
    relay_id uuid NOT NULL,
    attempt integer NOT NULL CHECK (attempt>0),
    reason text NOT NULL CHECK (reason IN ('dual_rtt','site','geoip','default_region','available')),
    combined_rtt_us integer CHECK (combined_rtt_us BETWEEN 0 AND 4000000),
    selected_at timestamptz NOT NULL,
    failed_at timestamptz,
    CHECK ((reason='dual_rtt')=(combined_rtt_us IS NOT NULL)),
    PRIMARY KEY (round_id,attempt),
    UNIQUE (round_id,relay_id),
    FOREIGN KEY (round_id,relay_id) REFERENCES session_relay_candidates(round_id,relay_id)
);
CREATE UNIQUE INDEX session_relay_active_selection
    ON session_relay_selections(round_id) WHERE failed_at IS NULL;

-- +goose Down
DROP TABLE session_relay_selections;
