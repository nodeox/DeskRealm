-- +goose Up
CREATE TABLE relay_network_policies (
    organization_id uuid PRIMARY KEY REFERENCES organizations(id),
    revision bigint NOT NULL CHECK (revision>0),
    policy jsonb NOT NULL CHECK (jsonb_typeof(policy)='object')
);

ALTER TABLE session_relay_rounds ADD COLUMN network_revision bigint NOT NULL DEFAULT 0 CHECK (network_revision>=0),
    ADD COLUMN controller_address inet,
    ADD COLUMN target_address inet;
UPDATE session_relay_rounds SET superseded_at=clock_timestamp() WHERE superseded_at IS NULL;
ALTER TABLE session_relay_rounds ADD CONSTRAINT relay_round_addresses CHECK (
    superseded_at IS NOT NULL OR
    (controller_address IS NOT NULL AND (target_address IS NULL)=(opened_at IS NULL)));

-- +goose Down
ALTER TABLE session_relay_rounds DROP CONSTRAINT relay_round_addresses, DROP COLUMN target_address,
    DROP COLUMN controller_address, DROP COLUMN network_revision;
DROP TABLE relay_network_policies;
