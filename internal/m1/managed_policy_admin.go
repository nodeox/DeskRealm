package m1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type policyContent struct {
	Name       string   `json:"name"`
	Actions    []string `json:"allowed_actions"`
	Unattended []string `json:"unattended_purposes"`
}

type publishedPolicy struct {
	policyContent
	Version int64     `json:"policy_version"`
	Schema  int16     `json:"schema_version"`
	SHA256  string    `json:"payload_sha256"`
	At      time.Time `json:"published_at"`
}

func (in *policyContent) normalize() bool {
	in.Name = strings.TrimSpace(in.Name)
	if !validOrganization(in.Name) || in.Actions == nil || (len(in.Actions) > 0 && !distinctControlActions(in.Actions)) ||
		len(in.Unattended) > 32 || (len(in.Unattended) != 0 && len(in.Actions) == 0) {
		return false
	}
	if in.Unattended == nil {
		in.Unattended = []string{}
	}
	slices.Sort(in.Actions)
	slices.Sort(in.Unattended)
	for i, purpose := range in.Unattended {
		if !connectionPurpose.MatchString(purpose) || (i > 0 && purpose == in.Unattended[i-1]) {
			return false
		}
	}
	return true
}

func policyPayloadHash(content policyContent) [32]byte {
	payload := append([]byte("DR-M2-policy-v1\x00"), controlActionMask(content.Actions))
	for _, purpose := range content.Unattended {
		payload = append(payload, purpose...)
		payload = append(payload, 0)
	}
	return sha256.Sum256(payload)
}

func (a *API) publishManagedPolicy(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var content policyContent
	if !readJSON(w, r, &content) {
		return
	}
	if !content.normalize() {
		failure(w, "publish managed policy", errPolicyInput)
		return
	}
	ctx := r.Context()
	hash := policyPayloadHash(content)
	result := publishedPolicy{policyContent: content, Schema: 1, SHA256: hex.EncodeToString(hash[:])}
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		err := tx.QueryRow(ctx, `INSERT INTO managed_policy_versions
			(organization_id,name,schema_version,allowed_actions,unattended_purposes,payload_sha256,published_by)
			VALUES($1,$2,1,$3,$4,$5,$6) RETURNING version,published_at`, who.Organization,
			content.Name, content.Actions, content.Unattended, hash[:], who.ID).Scan(&result.Version, &result.At)
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "managed_policy.published", "", strconv.FormatInt(result.Version, 10))
	})
	if err != nil {
		failure(w, "publish managed policy", err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (a *API) listManagedPolicies(w http.ResponseWriter, r *http.Request) {
	var before int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		var err error
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before <= 0 {
			failure(w, "list managed policies", errPolicyInput)
			return
		}
	}
	ctx := r.Context()
	items, err := readAsAdmin(ctx, a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]publishedPolicy, error) {
		rows, err := tx.Query(ctx, `SELECT version,name,schema_version,allowed_actions,unattended_purposes,
			encode(payload_sha256,'hex'),published_at FROM managed_policy_versions
			WHERE organization_id=$1 AND ($2::bigint=0 OR version<$2)
			ORDER BY version DESC LIMIT 101`, org, before)
		if err != nil {
			return nil, err
		}
		return pgx.AppendRows([]publishedPolicy{}, rows, func(row pgx.CollectableRow) (publishedPolicy, error) {
			var p publishedPolicy
			err := row.Scan(&p.Version, &p.Name, &p.Schema, &p.Actions, &p.Unattended, &p.SHA256, &p.At)
			return p, err
		})
	})
	if err != nil {
		failure(w, "list managed policies", err)
		return
	}
	var next int64
	if len(items) > 100 {
		items = items[:100]
		next = items[99].Version
	}
	writeJSON(w, http.StatusOK, struct {
		Policies []publishedPolicy `json:"policies"`
		Next     int64             `json:"next,omitempty"`
	}{items, next})
}

type policyAssignmentVersions struct {
	Policy  *int64 `json:"policy_version"`
	Canary  *int64 `json:"canary_version"`
	Percent int16  `json:"canary_percent"`
}

func (v policyAssignmentVersions) valid() bool {
	if v.Policy != nil && *v.Policy < 1 {
		return false
	}
	if v.Canary == nil {
		return v.Percent == 0
	}
	return *v.Canary >= 1 && v.Percent >= 1 && v.Percent <= 99 && (v.Policy == nil || *v.Policy != *v.Canary)
}

type policyAssignment struct {
	policyAssignmentVersions
	ID           string `json:"id"`
	Scope        string `json:"scope"`
	Object       string `json:"object_id,omitempty"`
	Revision     int64  `json:"revision"`
	RollbackFrom *int64 `json:"rollback_from_revision,omitempty"`
	RollbackTo   *int64 `json:"rollback_to_revision,omitempty"`
}

func (a *API) readManagedAssignments(w http.ResponseWriter, r *http.Request) {
	id, after := r.PathValue("id"), r.URL.Query().Get("after")
	var revision int64
	if raw := r.URL.Query().Get("revision"); raw != "" {
		var err error
		revision, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || revision < 1 || id == "" {
			failure(w, "read managed assignments", errPolicyInput)
			return
		}
	}
	if (id != "" && !userIDPattern.MatchString(id)) || (after != "" && (id != "" || !userIDPattern.MatchString(after))) {
		failure(w, "read managed assignments", errPolicyInput)
		return
	}
	ctx := r.Context()
	items, err := readAsAdmin(ctx, a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]policyAssignment, error) {
		rows, err := tx.Query(ctx, `SELECT a.id,a.scope,COALESCE(a.device_id::text,a.user_id::text,a.group_id::text,''),
			v.revision,v.policy_version,v.canary_version,v.canary_percent,v.rollback_from,v.rollback_to
			FROM managed_policy_assignments a JOIN managed_policy_assignment_revisions v ON v.assignment_id=a.id
			AND v.revision=COALESCE(NULLIF($4::bigint,0),a.revision) WHERE a.organization_id=$1
			AND ($2='' OR a.id=NULLIF($2,'')::uuid) AND ($3='' OR a.id>NULLIF($3,'')::uuid)
			ORDER BY a.id LIMIT 101`, org, id, after, revision)
		if err != nil {
			return nil, err
		}
		return pgx.AppendRows([]policyAssignment{}, rows, func(row pgx.CollectableRow) (policyAssignment, error) {
			var out policyAssignment
			err := row.Scan(&out.ID, &out.Scope, &out.Object, &out.Revision,
				&out.Policy, &out.Canary, &out.Percent, &out.RollbackFrom, &out.RollbackTo)
			return out, err
		})
	})
	if err != nil {
		failure(w, "read managed assignments", err)
		return
	}
	if id != "" {
		if len(items) == 0 {
			failure(w, "read managed assignments", errPolicyMissing)
			return
		}
		writeJSON(w, http.StatusOK, items[0])
		return
	}
	var next string
	if len(items) > 100 {
		items = items[:100]
		next = items[99].ID
	}
	writeJSON(w, http.StatusOK, struct {
		Assignments []policyAssignment `json:"assignments"`
		Next        string             `json:"next,omitempty"`
	}{items, next})
}

func policyTarget(scope, object string) (deviceID, userID, groupID string, valid bool) {
	if scope == "organization" {
		return "", "", "", object == ""
	}
	if !userIDPattern.MatchString(object) {
		return "", "", "", false
	}
	switch scope {
	case "device":
		return object, "", "", true
	case "user":
		return "", object, "", true
	case "group":
		return "", "", object, true
	default:
		return "", "", "", false
	}
}

func validatePolicyVersions(ctx context.Context, tx pgx.Tx, org, scope string, in policyAssignmentVersions) error {
	missingOrUnsafe := false
	err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM unnest(ARRAY[$2::bigint,$3::bigint]) AS choice(version)
		LEFT JOIN managed_policy_versions v ON v.organization_id=$1 AND v.version=choice.version
		WHERE choice.version IS NOT NULL AND (v.version IS NULL OR v.schema_version<>1
		OR ($4<>'device' AND cardinality(v.unattended_purposes)>0)))`, org, in.Policy, in.Canary, scope).Scan(&missingOrUnsafe)
	if err == nil && missingOrUnsafe {
		return errPolicyInput
	}
	return err
}

func appendAssignmentRevision(ctx context.Context, tx pgx.Tx, who actor, result policyAssignment, action string) error {
	if err := validatePolicyVersions(ctx, tx, who.Organization, result.Scope, result.policyAssignmentVersions); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO managed_policy_assignment_revisions
		(organization_id,assignment_id,revision,policy_version,canary_version,canary_percent,rollback_from,rollback_to,changed_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, who.Organization, result.ID, result.Revision,
		result.Policy, result.Canary, result.Percent, result.RollbackFrom, result.RollbackTo, who.ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE managed_policy_assignments SET revision=$2 WHERE id=$1`, result.ID, result.Revision); err != nil {
		return err
	}
	deviceID := ""
	if result.Scope == "device" {
		deviceID = result.Object
	}
	if err := refreshManagedPolicyTargets(ctx, tx, who.Organization, deviceID, false); err != nil {
		return err
	}
	return auditWithReason(ctx, tx, who.Organization, who.ID, action, result.ID, strconv.FormatInt(result.Revision, 10))
}

func (a *API) updateManagedAssignment(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var in struct {
		policyAssignmentVersions
		Scope    string `json:"scope"`
		Object   string `json:"object_id"`
		Revision int64  `json:"revision"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	deviceID, userID, groupID, valid := policyTarget(in.Scope, in.Object)
	id := r.PathValue("id")
	if !in.valid() || (id == "" && (!valid || in.Revision != 0 || (in.Policy == nil && in.Canary == nil))) ||
		(id != "" && (!userIDPattern.MatchString(id) || in.Scope != "" || in.Object != "" || in.Revision < 1)) {
		failure(w, "update managed assignment", errPolicyInput)
		return
	}
	ctx := r.Context()
	result := policyAssignment{policyAssignmentVersions: in.policyAssignmentVersions, ID: id,
		Scope: in.Scope, Object: in.Object, Revision: in.Revision + 1}
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		if id == "" {
			err := tx.QueryRow(ctx, `INSERT INTO managed_policy_assignments(organization_id,scope,device_id,user_id,group_id)
				VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid) RETURNING id`,
				who.Organization, in.Scope, deviceID, userID, groupID).Scan(&result.ID)
			var dbErr *pgconn.PgError
			switch {
			case uniqueViolation(err):
				return errPolicyStale
			case errors.As(err, &dbErr) && dbErr.Code == "23503":
				return errPolicyInput
			case err != nil:
				return err
			}
		} else {
			var err error
			result.Scope, result.Object, err = lockPolicyAssignment(ctx, tx, who.Organization, id, in.Revision)
			if err != nil {
				return err
			}
		}
		return appendAssignmentRevision(ctx, tx, who, result, "managed_policy.assigned")
	})
	if err != nil {
		failure(w, "update managed assignment", err)
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func lockPolicyAssignment(ctx context.Context, tx pgx.Tx, org, id string, revision int64) (string, string, error) {
	var scope, object string
	var current int64
	err := tx.QueryRow(ctx, `SELECT scope,COALESCE(device_id::text,user_id::text,group_id::text,''),revision
		FROM managed_policy_assignments WHERE organization_id=$1 AND id=$2 FOR NO KEY UPDATE`, org, id).
		Scan(&scope, &object, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errPolicyMissing
	}
	if err == nil && current != revision {
		err = errPolicyStale
	}
	return scope, object, err
}

func (a *API) rollbackManagedAssignment(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var in struct {
		Revision int64 `json:"revision"`
		To       int64 `json:"rollback_to_revision"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) || in.To < 1 || in.To >= in.Revision {
		failure(w, "rollback managed assignment", errPolicyInput)
		return
	}
	ctx := r.Context()
	result := policyAssignment{ID: id, Revision: in.Revision + 1, RollbackFrom: &in.Revision, RollbackTo: &in.To}
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		var err error
		result.Scope, result.Object, err = lockPolicyAssignment(ctx, tx, who.Organization, id, in.Revision)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT policy_version,canary_version,canary_percent FROM managed_policy_assignment_revisions
			WHERE organization_id=$1 AND assignment_id=$2 AND revision=$3`, who.Organization, id, in.To).
			Scan(&result.Policy, &result.Canary, &result.Percent)
		if errors.Is(err, pgx.ErrNoRows) {
			return errPolicyMissing
		}
		if err != nil {
			return err
		}
		return appendAssignmentRevision(ctx, tx, who, result, "managed_policy.rolled_back")
	})
	if err != nil {
		failure(w, "rollback managed assignment", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
