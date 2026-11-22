-- +goose Up
CREATE TABLE tasks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    kind text NOT NULL CHECK (kind IN ('user_import','device_enrollment')),
    owner_id uuid NOT NULL,
    device_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((kind = 'device_enrollment') = (device_id IS NOT NULL)),
    FOREIGN KEY (organization_id,owner_id) REFERENCES users(organization_id,id),
    FOREIGN KEY (organization_id,device_id) REFERENCES devices(organization_id,id),
    UNIQUE (device_id)
);
CREATE INDEX tasks_organization_cursor ON tasks(organization_id,created_at DESC,id DESC);
CREATE INDEX tasks_import_owner ON tasks(owner_id) WHERE kind='user_import';

CREATE TABLE task_targets (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id uuid NOT NULL REFERENCES tasks(id),
    object_key text NOT NULL,
    state text NOT NULL CHECK (state IN ('waiting','queued','running','succeeded','failed')),
    result_code text NOT NULL DEFAULT '',
    retryable boolean NOT NULL DEFAULT false,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 5),
    requested_by uuid REFERENCES users(id),
    lease_token uuid,
    lease_until timestamptz,
    CHECK ((lease_token IS NULL) = (lease_until IS NULL)),
    CHECK ((state='running') = (lease_token IS NOT NULL)),
    CHECK (state NOT IN ('queued','running') OR requested_by IS NOT NULL),
    CHECK (NOT retryable OR state='failed'),
    UNIQUE (task_id,object_key)
);
CREATE INDEX task_targets_claim ON task_targets(state,lease_until,id)
    WHERE state IN ('queued','running');

CREATE TABLE task_attempts (
    id uuid PRIMARY KEY,
    target_id uuid NOT NULL REFERENCES task_targets(id),
    requested_by uuid NOT NULL REFERENCES users(id),
    sequence integer NOT NULL CHECK (sequence BETWEEN 0 AND 5),
    result_code text,
    final_version bigint,
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    UNIQUE (target_id,sequence),
    CHECK ((result_code IS NULL) = (finished_at IS NULL)),
    CHECK (final_version IS NULL OR finished_at IS NOT NULL)
);

INSERT INTO tasks(id,organization_id,kind,owner_id,created_at)
SELECT id,organization_id,'user_import',created_by,created_at FROM identity_imports;
INSERT INTO task_targets(task_id,object_key,state,result_code,retryable)
SELECT i.id,entry->>'username',CASE WHEN entry->>'result'='import_failed' THEN 'failed' ELSE 'succeeded' END,
    entry->>'result',entry->>'result'='import_failed'
FROM identity_imports i CROSS JOIN LATERAL jsonb_array_elements(i.result) entry;

INSERT INTO tasks(organization_id,kind,owner_id,device_id,created_at)
SELECT d.organization_id,'device_enrollment',c.issued_by,d.id,d.created_at
FROM devices d JOIN enrollment_attempts a ON a.device_id=d.id
JOIN enrollment_credentials c ON c.id=a.credential_id;
INSERT INTO task_targets(task_id,object_key,state,result_code)
SELECT t.id,a.rustdesk_id,CASE r.decision WHEN 'approved' THEN 'succeeded' WHEN 'rejected' THEN 'failed'
    ELSE CASE WHEN d.registration_state='retired' THEN 'failed' ELSE 'waiting' END END,
    COALESCE(r.decision, CASE WHEN d.registration_state='retired' THEN 'retired' ELSE 'pending_approval' END)
FROM tasks t JOIN devices d ON d.id=t.device_id JOIN registration_approvals r ON r.device_id=d.id
JOIN enrollment_attempts a ON a.device_id=d.id;

INSERT INTO task_attempts(id,target_id,sequence,requested_by,result_code,final_version,started_at,finished_at)
SELECT gen_random_uuid(),tt.id,0,t.owner_id,tt.result_code,tt.version,t.created_at,t.created_at
FROM task_targets tt JOIN tasks t ON t.id=tt.task_id WHERE t.kind='user_import'
UNION ALL
SELECT gen_random_uuid(),tt.id,0,r.decided_by,tt.result_code,tt.version,r.decided_at,r.decided_at
FROM task_targets tt JOIN tasks t ON t.id=tt.task_id
JOIN registration_approvals r ON r.device_id=t.device_id WHERE r.decided_at IS NOT NULL;

-- +goose Down
DROP TABLE task_attempts,task_targets,tasks;
