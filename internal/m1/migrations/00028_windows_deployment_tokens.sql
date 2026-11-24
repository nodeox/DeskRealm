-- +goose Up
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_action_check, DROP CONSTRAINT api_tokens_scope_check,
    ADD COLUMN device_group_id uuid,
    ADD CONSTRAINT api_tokens_action_scope CHECK (
        (action='users.read' AND scope IN ('organization','group','self') AND device_group_id IS NULL) OR
        (action='windows.deploy' AND scope='device_group' AND device_group_id IS NOT NULL)
    ),
    ADD FOREIGN KEY (organization_id,device_group_id) REFERENCES device_groups(organization_id,id);

ALTER TABLE windows_enrollment_bindings ADD COLUMN api_token_id uuid REFERENCES api_tokens(id),
    ADD CHECK (api_token_id IS NULL OR NOT auto_approve);

-- +goose Down
UPDATE enrollment_credentials e SET expires_at=LEAST(e.expires_at,clock_timestamp())
    FROM windows_enrollment_bindings b WHERE b.credential_id=e.id AND b.api_token_id IS NOT NULL;
ALTER TABLE windows_enrollment_bindings DROP COLUMN api_token_id;
DELETE FROM api_tokens WHERE action='windows.deploy';
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_action_scope, DROP COLUMN device_group_id,
    ADD CONSTRAINT api_tokens_action_check CHECK (action='users.read'),
    ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('organization','group','self'));
