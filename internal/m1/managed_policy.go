package m1

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	errPolicyInput   = errors.New("invalid_managed_policy_request")
	errPolicyMissing = errors.New("managed_policy_not_found")
	errPolicyStale   = errors.New("managed_policy_stale")
)

// The organization lock used by policy writers and device lifecycle mutations
// serializes selections. Native receipts only lock their device row.
func refreshManagedPolicyTargets(ctx context.Context, tx pgx.Tx, org, deviceID string, retry bool) error {
	rows, err := tx.Query(ctx, `WITH desired AS MATERIALIZED (
		SELECT d.id,d.organization_id,d.device_key_epoch,c.policy_version,c.assignment_id,c.revision
		FROM devices d
		LEFT JOIN managed_policy_deliveries current ON current.id=d.managed_policy_delivery_id
		LEFT JOIN LATERAL (
			SELECT candidate.assignment_id,candidate.revision,candidate.policy_version
			FROM (
				SELECT a.id AS assignment_id,a.revision,
					CASE WHEN r.canary_version IS NOT NULL AND bucket.value % 100 < r.canary_percent
						THEN r.canary_version ELSE r.policy_version END AS policy_version,
					CASE a.scope WHEN 'device' THEN 1 WHEN 'user' THEN 2 WHEN 'group' THEN 3 ELSE 4 END AS priority
				FROM managed_policy_assignments a
				JOIN managed_policy_assignment_revisions r ON r.assignment_id=a.id AND r.revision=a.revision
				CROSS JOIN LATERAL (SELECT sha256(uuid_send(a.id)||uuid_send(d.id)) AS bytes) h
				CROSS JOIN LATERAL (SELECT get_byte(h.bytes,0)::bigint*16777216+get_byte(h.bytes,1)*65536
					+get_byte(h.bytes,2)*256+get_byte(h.bytes,3) AS value) bucket
				WHERE a.organization_id=d.organization_id AND d.registration_state<>'retired'
				AND (a.device_id=d.id OR a.user_id=d.managed_policy_owner_id OR a.group_id=d.group_id OR a.scope='organization')
			) candidate WHERE candidate.policy_version IS NOT NULL ORDER BY candidate.priority LIMIT 1
		) c ON true
		WHERE d.organization_id=$1 AND ($2='' OR d.id=NULLIF($2,'')::uuid)
		AND (c.policy_version IS DISTINCT FROM current.policy_version OR
			(c.policy_version IS NOT NULL AND (current.key_epoch<>d.device_key_epoch OR $3)))
	), issued AS (
		INSERT INTO managed_policy_deliveries(organization_id,device_id,policy_version,key_epoch,assignment_id,assignment_revision)
		SELECT organization_id,id,policy_version,device_key_epoch,assignment_id,revision FROM desired WHERE policy_version IS NOT NULL
		RETURNING id,device_id
	)
	UPDATE devices d SET managed_policy_delivery_id=issued.id,expected_policy_version=desired.policy_version
	FROM desired LEFT JOIN issued ON issued.device_id=desired.id WHERE d.id=desired.id RETURNING d.id`, org, deviceID, retry)
	if err != nil {
		return err
	}
	changed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(changed) == 0 {
		return err
	}
	return recheckSessions(ctx, tx, "organization_id=$1 AND target_device_id=ANY($2::uuid[])", org, changed)
}

type policyDelivery struct {
	ID           string   `json:"delivery_id"`
	Organization string   `json:"organization_id"`
	Device       string   `json:"device_id"`
	KeyEpoch     int64    `json:"key_epoch"`
	Version      int64    `json:"policy_version"`
	Schema       int16    `json:"schema_version"`
	SHA256       string   `json:"payload_sha256"`
	Actions      []string `json:"allowed_actions"`
	Unattended   []string `json:"unattended_purposes"`
}

type devicePolicyStatus struct {
	Owner           string     `json:"owner_user_id,omitempty"`
	DeviceVersion   int64      `json:"device_version"`
	DeliveryID      string     `json:"delivery_id,omitempty"`
	ExpectedVersion *int64     `json:"expected_policy_version"`
	AppliedVersion  *int64     `json:"applied_policy_version"`
	State           string     `json:"state"`
	FailedFields    []string   `json:"failed_fields,omitempty"`
	ReceivedAt      *time.Time `json:"received_at,omitempty"`
}

func currentPolicyDelivery(ctx context.Context, tx pgx.Tx, d runtimeDevice) (*policyDelivery, error) {
	var result policyDelivery
	var sum []byte
	err := tx.QueryRow(ctx, `SELECT p.id,p.policy_version,v.schema_version,v.payload_sha256,v.allowed_actions,v.unattended_purposes
		FROM devices d JOIN managed_policy_deliveries p ON p.id=d.managed_policy_delivery_id
		JOIN managed_policy_versions v ON v.organization_id=p.organization_id AND v.version=p.policy_version
		WHERE d.id=$1 AND p.key_epoch=$2`, d.ID, d.Epoch).
		Scan(&result.ID, &result.Version, &result.Schema, &sum, &result.Actions, &result.Unattended)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result.Organization, result.Device, result.KeyEpoch = d.Organization, d.ID, d.Epoch
	result.SHA256 = hex.EncodeToString(sum)
	return &result, nil
}

type managedPolicyCapabilities struct {
	Version    int64
	Actions    []string
	Unattended bool
}

// effectiveManagedPolicy is usable inside session authorization transactions.
// Old applied versions remain visible to admins but cannot authorize a session.
func effectiveManagedPolicy(ctx context.Context, tx pgx.Tx, org, deviceID, purpose string) (managedPolicyCapabilities, bool, error) {
	var result managedPolicyCapabilities
	err := tx.QueryRow(ctx, `SELECT v.version,v.allowed_actions,$3=ANY(v.unattended_purposes)
		FROM devices d JOIN managed_policy_deliveries p ON p.id=d.managed_policy_delivery_id
		JOIN managed_policy_receipts r ON r.delivery_id=p.id AND r.result='applied'
		JOIN managed_policy_versions v ON v.organization_id=p.organization_id AND v.version=p.policy_version
		WHERE d.organization_id=$1 AND d.id=$2 AND d.registration_state='approved'
		AND d.device_key_epoch=p.key_epoch AND v.schema_version=1`, org, deviceID, purpose).
		Scan(&result.Version, &result.Actions, &result.Unattended)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, false, nil
	}
	return result, err == nil, err
}
