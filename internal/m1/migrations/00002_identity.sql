-- +goose Up
ALTER TABLE audit_events ADD COLUMN reason text CHECK (char_length(reason) <= 200);

-- +goose Down
ALTER TABLE audit_events DROP COLUMN reason;
