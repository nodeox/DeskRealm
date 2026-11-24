-- +goose Up
ALTER TABLE windows_releases ADD COLUMN installer_publication jsonb
    CHECK (jsonb_typeof(installer_publication)='object');

-- +goose Down
ALTER TABLE windows_releases DROP COLUMN installer_publication;
