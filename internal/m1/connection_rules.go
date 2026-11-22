package m1

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	errConnectionInput   = errors.New("invalid_connection_rule_request")
	errConnectionMissing = errors.New("connection_rule_not_found")
	errConnectionStale   = errors.New("connection_rule_stale")
)

type connectionRuleSpec struct {
	Name        string   `json:"name"`
	SourceUser  string   `json:"source_user_id,omitempty"`
	SourceGroup string   `json:"source_group_id,omitempty"`
	Controller  string   `json:"controller_device_id,omitempty"`
	Target      string   `json:"target_device_id,omitempty"`
	TargetGroup string   `json:"target_group_id,omitempty"`
	Purpose     string   `json:"purpose"`
	Effect      string   `json:"effect"`
	Role        string   `json:"control_role_id,omitempty"`
	RoleVersion int64    `json:"control_role_version,omitempty"`
	Actions     []string `json:"allowed_actions"`
}

type connectionRule struct {
	connectionRuleSpec
	ID        string `json:"id"`
	Version   int64  `json:"version"`
	Latest    int64  `json:"latest_version"`
	Published *int64 `json:"published_version"`
}

func (in connectionRuleSpec) valid() bool {
	if !validOrganization(in.Name) || !connectionPurpose.MatchString(in.Purpose) ||
		(in.SourceUser == "") == (in.SourceGroup == "") || (in.Target == "") == (in.TargetGroup == "") {
		return false
	}
	for _, id := range []string{in.SourceUser, in.SourceGroup, in.Controller, in.Target, in.TargetGroup} {
		if id != "" && !userIDPattern.MatchString(id) {
			return false
		}
	}
	switch in.Effect {
	case "deny":
		return in.Role == "" && in.RoleVersion == 0 && len(in.Actions) == 0
	case "allow":
		return userIDPattern.MatchString(in.Role) && in.RoleVersion > 0 && distinctControlActions(in.Actions)
	default:
		return false
	}
}

const readConnectionRules = `SELECT h.id,v.version,h.latest_version,h.published_version,v.name,
	COALESCE(v.source_user_id::text,''),COALESCE(v.source_group_id::text,''),COALESCE(v.controller_device_id::text,''),
	COALESCE(v.target_device_id::text,''),COALESCE(v.target_group_id::text,''),v.purpose,v.effect,
	COALESCE(v.role_id::text,''),COALESCE(v.role_version,0),v.allowed_actions
	FROM connection_rules h JOIN connection_rule_versions v ON v.organization_id=h.organization_id AND v.rule_id=h.id
	WHERE h.organization_id=$1 AND h.archived_at IS NULL`

func scanConnectionRule(row pgx.CollectableRow) (connectionRule, error) {
	var out connectionRule
	err := row.Scan(&out.ID, &out.Version, &out.Latest, &out.Published, &out.Name,
		&out.SourceUser, &out.SourceGroup, &out.Controller, &out.Target, &out.TargetGroup,
		&out.Purpose, &out.Effect, &out.Role, &out.RoleVersion, &out.Actions)
	return out, err
}

func (a *API) readConnectionRules(w http.ResponseWriter, r *http.Request) {
	id, after := r.PathValue("id"), r.URL.Query().Get("after")
	var version int64
	if raw := r.URL.Query().Get("version"); raw != "" {
		var err error
		version, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || version < 1 || id == "" {
			failure(w, "read connection rules", errConnectionInput)
			return
		}
	}
	if (id != "" && !userIDPattern.MatchString(id)) || (after != "" && (id != "" || !userIDPattern.MatchString(after))) {
		failure(w, "read connection rules", errConnectionInput)
		return
	}
	ctx := r.Context()
	items, err := readAsAdmin(ctx, a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]connectionRule, error) {
		rows, err := tx.Query(ctx, readConnectionRules+`
			AND ($2='' OR h.id=NULLIF($2,'')::uuid) AND ($3='' OR h.id>NULLIF($3,'')::uuid)
			AND v.version=COALESCE(NULLIF($4::bigint,0),h.latest_version)
			ORDER BY h.id LIMIT 101`, org, id, after, version)
		if err != nil {
			return nil, err
		}
		return pgx.AppendRows(make([]connectionRule, 0), rows, scanConnectionRule)
	})
	if err != nil {
		failure(w, "read connection rules", err)
		return
	}
	if id != "" {
		if len(items) == 0 {
			failure(w, "read connection rule", errConnectionMissing)
			return
		}
		writeJSON(w, http.StatusOK, items[0])
		return
	}
	var next string
	if len(items) > 100 {
		items = items[:100]
		next = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, struct {
		Rules []connectionRule `json:"rules"`
		Next  string           `json:"next,omitempty"`
	}{items, next})
}

func validateRuleRole(ctx context.Context, tx pgx.Tx, org string, spec connectionRuleSpec) error {
	if spec.Effect == "deny" {
		return nil
	}
	var includesAll bool
	err := tx.QueryRow(ctx, `SELECT allowed_actions @> $4::text[] FROM control_roles
		WHERE organization_id=$1 AND id=$2 AND version=$3`, org, spec.Role, spec.RoleVersion, spec.Actions).Scan(&includesAll)
	if errors.Is(err, pgx.ErrNoRows) {
		return errControlRoleVersion
	}
	if err == nil && !includesAll {
		return errConnectionInput
	}
	return err
}

// lockConnectionRule checks the revision after obtaining the rule row lock.
// Admin mutations acquire the organization authorization lock before this.
func lockConnectionRule(ctx context.Context, tx pgx.Tx, org, id string, version int64) (*int64, error) {
	var latest int64
	var published *int64
	err := tx.QueryRow(ctx, `SELECT latest_version,published_version FROM connection_rules
		WHERE organization_id=$1 AND id=$2 AND archived_at IS NULL FOR NO KEY UPDATE`, org, id).
		Scan(&latest, &published)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errConnectionMissing
	}
	if err == nil && version != latest {
		return nil, errConnectionStale
	}
	return published, err
}

func (a *API) saveConnectionRule(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	var input struct {
		connectionRuleSpec
		Version int64 `json:"version"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if !input.valid() || (id == "" && input.Version != 0) ||
		(id != "" && (!userIDPattern.MatchString(id) || input.Version < 1)) {
		failure(w, "save connection rule", errConnectionInput)
		return
	}
	if input.Actions == nil {
		input.Actions = []string{}
	}
	slices.Sort(input.Actions)
	ctx := r.Context()
	result := connectionRule{connectionRuleSpec: input.connectionRuleSpec, ID: id, Version: input.Version + 1, Latest: input.Version + 1}
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		if err := validateRuleRole(ctx, tx, who.Organization, input.connectionRuleSpec); err != nil {
			return err
		}
		action := "connection_rule.created"
		if id == "" {
			if err := tx.QueryRow(ctx, `INSERT INTO connection_rules(organization_id) VALUES($1) RETURNING id`,
				who.Organization).Scan(&result.ID); err != nil {
				return err
			}
		} else {
			action = "connection_rule.edited"
			var err error
			result.Published, err = lockConnectionRule(ctx, tx, who.Organization, id, input.Version)
			if err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO connection_rule_versions(organization_id,rule_id,version,name,
			source_user_id,source_group_id,controller_device_id,target_device_id,target_group_id,purpose,effect,
			role_id,role_version,allowed_actions,created_by)
			VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,
			NULLIF($8,'')::uuid,NULLIF($9,'')::uuid,$10,$11,NULLIF($12,'')::uuid,NULLIF($13::bigint,0),$14,$15)`,
			who.Organization, result.ID, result.Version, result.Name, result.SourceUser, result.SourceGroup, result.Controller,
			result.Target, result.TargetGroup, result.Purpose, result.Effect, result.Role, result.RoleVersion, result.Actions, who.ID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return errConnectionInput
		}
		if err != nil {
			return err
		}
		if id != "" {
			if _, err := tx.Exec(ctx, `UPDATE connection_rules SET latest_version=$2 WHERE id=$1`, id, result.Version); err != nil {
				return err
			}
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, action, result.ID, strconv.FormatInt(result.Version, 10))
	})
	if err != nil {
		failure(w, "save connection rule", err)
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (a *API) changeConnectionRule(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id, change := r.PathValue("id"), r.PathValue("change")
	if change != "publish" && change != "archive" {
		failure(w, "change connection rule", errConnectionMissing)
		return
	}
	var in struct {
		Version int64 `json:"version"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Version < 1 || !userIDPattern.MatchString(id) {
		failure(w, "change connection rule", errConnectionInput)
		return
	}
	ctx := r.Context()
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		published, err := lockConnectionRule(ctx, tx, who.Organization, id, in.Version)
		if err != nil {
			return err
		}
		action := "connection_rule.archived"
		if change == "publish" {
			action = "connection_rule.published"
			var spec connectionRuleSpec
			err = tx.QueryRow(ctx, `SELECT effect,COALESCE(role_id::text,''),COALESCE(role_version,0),allowed_actions
				FROM connection_rule_versions WHERE organization_id=$1 AND rule_id=$2 AND version=$3`,
				who.Organization, id, in.Version).Scan(&spec.Effect, &spec.Role, &spec.RoleVersion, &spec.Actions)
			if err != nil {
				return err
			}
			if err := validateRuleRole(ctx, tx, who.Organization, spec); err != nil {
				return err
			}
			if published != nil && *published == in.Version {
				return nil
			}
			_, err = tx.Exec(ctx, `UPDATE connection_rules SET published_version=$2 WHERE id=$1`, id, in.Version)
		} else {
			_, err = tx.Exec(ctx, `UPDATE connection_rules SET archived_at=clock_timestamp() WHERE id=$1`, id)
		}
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, action, id, strconv.FormatInt(in.Version, 10))
	})
	if err != nil {
		failure(w, "change connection rule", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) previewConnectionRule(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var in connectionRequest
	if !readJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	var decision connectionDecision
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		var err error
		decision, err = evaluateConnectionRules(ctx, tx, who.Organization, in)
		if err != nil {
			return err
		}
		result := "denied"
		if decision.Allowed {
			result = "allowed"
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID,
			ResourceID: decision.Rule, Action: "connection_rule.previewed", Result: result, Reason: decision.Reason})
		return err
	})
	if err != nil {
		failure(w, "preview connection rule", err)
		return
	}
	writeJSON(w, http.StatusOK, decision)
}
