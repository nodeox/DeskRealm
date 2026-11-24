package m1

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var errAPIToken = errors.New("invalid_api_token")

type apiTokenRecord struct {
	ID            string     `json:"id"`
	Purpose       string     `json:"purpose"`
	Action        string     `json:"action"`
	Scope         string     `json:"scope"`
	GroupID       string     `json:"group_id,omitempty"`
	DeviceGroupID string     `json:"device_group_id,omitempty"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

type apiTokenInput struct {
	Purpose         string `json:"purpose"`
	Action          string `json:"action"`
	Scope           string `json:"scope"`
	GroupID         string `json:"group_id"`
	DeviceGroupID   string `json:"device_group_id"`
	LifetimeMinutes int    `json:"lifetime_minutes"`
}

type userReadCredentials struct {
	session string
	bearer  string
	fromAPI bool
}

func (a *API) readCredentials(r *http.Request) userReadCredentials {
	headers := r.Header.Values("Authorization")
	if len(headers) == 0 {
		return userReadCredentials{session: a.cookieValue(r)}
	}
	result := userReadCredentials{fromAPI: true}
	if len(headers) == 1 {
		scheme, value, _ := strings.Cut(headers[0], " ")
		if strings.EqualFold(scheme, "Bearer") {
			result.bearer = value
		}
	}
	return result
}

const tokenAllowed = `($6='' OR $6='organization' OR ($6='self' AND u.id=$2) OR
	($6='group' AND EXISTS (SELECT 1 FROM user_group_members tm WHERE tm.group_id=NULLIF($7,'')::uuid AND tm.user_id=u.id)))`

func (credentials userReadCredentials) authenticate(ctx context.Context, tx pgx.Tx) (actor, string, string, error) {
	if !credentials.fromAPI {
		result, err := actorSession(ctx, tx, credentials.session, false)
		return result, "", "", err
	}
	var result actor
	if !validToken(credentials.bearer) {
		return result, "", "", errAPIToken
	}
	var scope, group string
	err := tx.QueryRow(ctx, `SELECT u.id,u.organization_id,`+adminBinding+`,t.scope,COALESCE(t.group_id::text,'')
		FROM api_tokens t JOIN users u ON u.id=t.user_id
		WHERE t.digest=$1 AND t.action='users.read' AND t.revoked_at IS NULL AND t.expires_at>clock_timestamp()
		AND u.status='active'`, digest(credentials.bearer)).Scan(&result.ID, &result.Organization, &result.PlatformAdmin, &scope, &group)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errAPIToken
	}
	return result, scope, group, err
}

func canIssueToken(ctx context.Context, tx pgx.Tx, current actor, scope, group string) (bool, error) {
	if current.PlatformAdmin {
		return true, nil
	}
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM management_grants g JOIN management_role_actions a ON a.role=g.role
		WHERE g.user_id=$1 AND g.organization_id=$2 AND a.action='users.read'
		  AND (g.scope='organization' OR (g.scope=$3 AND g.group_id IS NOT DISTINCT FROM NULLIF($4,'')::uuid))
	)`, current.ID, current.Organization, scope, group).Scan(&allowed)
	return allowed, err
}

func (a *API) issueToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	var in apiTokenInput
	if !readJSON(w, r, &in) {
		return
	}
	in.Purpose = strings.TrimSpace(in.Purpose)
	if !validOrganization(in.Purpose) || in.LifetimeMinutes < 1 || in.LifetimeMinutes > 10080 || !in.validScope() {
		writeError(w, 400, "invalid_api_token_request")
		return
	}
	value, err := token()
	if err != nil {
		failure(w, "issue API token", err)
		return
	}
	result := apiTokenRecord{Purpose: in.Purpose, Action: in.Action, Scope: in.Scope, GroupID: in.GroupID,
		DeviceGroupID: in.DeviceGroupID}
	ctx := r.Context()
	err = pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		current, err := actorSession(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		if in.Scope == "group" {
			if err := requireGroup(ctx, tx, current.Organization, in.GroupID); err != nil {
				return err
			}
		}
		var allowed bool
		if in.Action == "windows.deploy" {
			allowed, err = canUseWindowsDeploymentGroup(ctx, tx, current, in.DeviceGroupID)
		} else {
			allowed, err = canIssueToken(ctx, tx, current, in.Scope, in.GroupID)
		}
		if err != nil {
			return err
		}
		if !allowed {
			return errForbidden
		}
		err = tx.QueryRow(ctx, `INSERT INTO api_tokens(digest,organization_id,user_id,purpose,action,scope,group_id,device_group_id,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,NULLIF($8,'')::uuid,clock_timestamp()+make_interval(mins=>$9))
			RETURNING id,expires_at`, digest(value), current.Organization, current.ID, in.Purpose, in.Action, in.Scope,
			in.GroupID, in.DeviceGroupID, in.LifetimeMinutes).
			Scan(&result.ID, &result.ExpiresAt)
		if err != nil {
			return err
		}
		return audit(ctx, tx, current.Organization, current.ID, "api_token.issued", result.ID)
	})
	if err != nil {
		failure(w, "issue API token", err)
		return
	}
	writeJSON(w, 201, struct {
		apiTokenRecord
		Token string `json:"token"`
	}{result, value})
}

func (in apiTokenInput) validScope() bool {
	switch in.Action {
	case "windows.deploy":
		return in.Scope == "device_group" && in.GroupID == "" && userIDPattern.MatchString(in.DeviceGroupID)
	case "users.read":
		if in.DeviceGroupID != "" {
			return false
		}
		if in.Scope == "group" {
			return userIDPattern.MatchString(in.GroupID)
		}
		return in.GroupID == "" && (in.Scope == "organization" || in.Scope == "self")
	default:
		return false
	}
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var result []apiTokenRecord
	err := pgx.BeginTxFunc(ctx, a.store.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		current, err := actorSession(ctx, tx, a.cookieValue(r), false)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,purpose,action,scope,COALESCE(group_id::text,''),COALESCE(device_group_id::text,''),expires_at,revoked_at
			FROM api_tokens WHERE organization_id=$1 AND (user_id=$2 OR $3)
			ORDER BY created_at DESC,id LIMIT 501`, current.Organization, current.ID, current.PlatformAdmin)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, pgx.RowToStructByPos[apiTokenRecord])
		return err
	})
	if err != nil {
		failure(w, "list API tokens", err)
		return
	}
	if result == nil {
		result = []apiTokenRecord{}
	}
	writeJSON(w, 200, result)
}

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		writeError(w, 404, "not_found")
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		current, err := actorSession(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at=now() WHERE id=$1 AND organization_id=$2
			AND (user_id=$3 OR $4) AND revoked_at IS NULL`, id, current.Organization, current.ID, current.PlatformAdmin)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return audit(ctx, tx, current.Organization, current.ID, "api_token.revoked", id)
	})
	if err != nil {
		failure(w, "revoke API token", err)
		return
	}
	w.WriteHeader(204)
}
