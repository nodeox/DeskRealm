package m1

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
)

var connectionPurpose = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type connectionRequest struct {
	User       string   `json:"user_id"`
	Controller string   `json:"controller_device_id"`
	Target     string   `json:"target_device_id"`
	RustDeskID string   `json:"target_rustdesk_id"`
	Book       string   `json:"book_id"`
	Entry      string   `json:"entry_id"`
	Purpose    string   `json:"purpose"`
	Actions    []string `json:"requested_actions"`
}

type connectionDecision struct {
	Allowed     bool     `json:"allowed"`
	Reason      string   `json:"reason"`
	Target      string   `json:"target_device_id,omitempty"`
	Rule        string   `json:"rule_id,omitempty"`
	RuleVersion int64    `json:"rule_version,omitempty"`
	Role        string   `json:"control_role_id,omitempty"`
	RoleVersion int64    `json:"control_role_version,omitempty"`
	Actions     []string `json:"allowed_actions,omitempty"`
}

func (in connectionRequest) valid() bool {
	if !userIDPattern.MatchString(in.User) || !userIDPattern.MatchString(in.Controller) ||
		!connectionPurpose.MatchString(in.Purpose) || !distinctControlActions(in.Actions) {
		return false
	}
	selectors := 0
	if in.Target != "" {
		if !userIDPattern.MatchString(in.Target) {
			return false
		}
		selectors++
	}
	if in.RustDeskID != "" {
		if !rustdeskIDPattern.MatchString(in.RustDeskID) {
			return false
		}
		selectors++
	}
	if in.Book != "" || in.Entry != "" {
		if !userIDPattern.MatchString(in.Book) || !userIDPattern.MatchString(in.Entry) {
			return false
		}
		selectors++
	}
	return selectors == 1
}

const connectionDeviceReady = managedDeviceOnline + ` AND d.managed_heartbeat_protocol=$5`

// evaluateConnectionRules checks the current ACL only. A caller that grants
// sessions must derive the subject and controller from its authenticated
// identity, serialize authorization mutations, and intersect the applied policy.
func evaluateConnectionRules(ctx context.Context, tx pgx.Tx, org string, in connectionRequest) (connectionDecision, error) {
	return evaluateConnectionRuleVersion(ctx, tx, org, in, "", 0)
}

func evaluateConnectionRuleVersion(ctx context.Context, tx pgx.Tx, org string, in connectionRequest, rule string, version int64) (connectionDecision, error) {
	if !in.valid() {
		return connectionDecision{}, errConnectionInput
	}
	who := actor{ID: in.User, Organization: org}
	err := tx.QueryRow(ctx, `SELECT `+adminBinding+` FROM users u
		WHERE u.organization_id=$1 AND u.id=$2 AND u.status='active'`, org, in.User).Scan(&who.PlatformAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return connectionDecision{Reason: "subject_inactive"}, nil
	}
	if err != nil {
		return connectionDecision{}, err
	}
	target := in.Target
	if in.Book != "" {
		target, err = resolveAddressBookDevice(ctx, tx, who, in.Book, in.Entry)
		switch {
		case errors.Is(err, errBookMissing), errors.Is(err, errEntryMissing), errors.Is(err, errEntryInactive):
			return connectionDecision{Reason: "address_book_unavailable"}, nil
		case err != nil:
			return connectionDecision{}, err
		}
	}
	var group string
	var controllerReady bool
	err = tx.QueryRow(ctx, `SELECT d.id,d.group_id,EXISTS(
		SELECT 1 FROM devices d WHERE d.organization_id=$1 AND d.id=$2 AND `+connectionDeviceReady+`
	) FROM devices d WHERE d.organization_id=$1 AND d.id<>$2 AND `+connectionDeviceReady+`
	AND (($3<>'' AND d.id=NULLIF($3,'')::uuid) OR ($4<>'' AND d.rustdesk_id=$4))`,
		org, in.Controller, target, in.RustDeskID, managedProtocolV1).Scan(&target, &group, &controllerReady)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !controllerReady) {
		return connectionDecision{Reason: "endpoint_unavailable"}, nil
	}
	if err != nil {
		return connectionDecision{}, err
	}

	var result connectionDecision
	err = tx.QueryRow(ctx, `SELECT v.effect='allow',v.rule_id,v.version,COALESCE(v.role_id::text,''),COALESCE(v.role_version,0)
		FROM connection_rules r JOIN connection_rule_versions v
			ON v.organization_id=r.organization_id AND v.rule_id=r.id AND v.version=r.published_version
		LEFT JOIN control_roles role ON role.organization_id=v.organization_id AND role.id=v.role_id AND role.version=v.role_version
		WHERE r.organization_id=$1 AND r.archived_at IS NULL AND v.purpose=$2
		AND (v.source_user_id=$3 OR EXISTS(
			SELECT 1 FROM user_group_members m WHERE m.organization_id=$1 AND m.group_id=v.source_group_id AND m.user_id=$3
		))
		AND (v.controller_device_id IS NULL OR v.controller_device_id=$4)
		AND (v.target_device_id=$5 OR v.target_group_id=$6)
		AND (v.effect='deny' OR (role.id IS NOT NULL AND v.allowed_actions @> $7::text[] AND role.allowed_actions @> $7::text[]
		AND ($8='' OR (v.rule_id=NULLIF($8,'')::uuid AND v.version=$9))))
		ORDER BY (v.effect='deny') DESC,r.id LIMIT 1`, org, in.Purpose, in.User, in.Controller, target, group, in.Actions, rule, version).
		Scan(&result.Allowed, &result.Rule, &result.RuleVersion, &result.Role, &result.RoleVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return connectionDecision{Reason: "no_matching_allow"}, nil
	}
	if err != nil {
		return connectionDecision{}, err
	}
	result.Reason = "explicit_deny"
	if result.Allowed {
		result.Reason = "matched_allow"
		result.Target = target
		result.Actions = in.Actions
	}
	return result, nil
}
