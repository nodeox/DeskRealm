-- +goose Up
CREATE TABLE relay_upgrades (
    task_id uuid PRIMARY KEY REFERENCES tasks(id),
    node_id uuid NOT NULL REFERENCES relay_nodes(id),
    candidate_sha text NOT NULL CHECK (candidate_sha ~ '^[a-f0-9]{64}$'),
    original_sha text CHECK (original_sha ~ '^[a-f0-9]{64}$' AND original_sha<>candidate_sha),
    ready_at timestamptz NOT NULL,
    claimed_at timestamptz,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','claimed','succeeded','rolled_back','canceled')),
    CHECK ((claimed_at IS NULL)=(original_sha IS NULL)),
    CHECK ((state IN ('claimed','succeeded','rolled_back'))=(claimed_at IS NOT NULL))
);
CREATE UNIQUE INDEX relay_upgrades_held ON relay_upgrades(node_id) WHERE state IN ('pending','claimed');
CREATE INDEX managed_session_redemptions_relay_node ON managed_session_redemptions(relay_id) WHERE path='relay';

-- +goose Down
DROP INDEX managed_session_redemptions_relay_node;
DROP TABLE relay_upgrades;
