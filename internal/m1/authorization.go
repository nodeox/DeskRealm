package m1

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type actor struct {
	ID            string
	Organization  string
	PlatformAdmin bool
}

// A user grant must apply to the same object on both list and direct API paths.
// Delegated roles cannot change a platform administrator account.
const userAllowed = `($4 OR (
	($3 <> 'users.state' OR NOT ` + adminBinding + `)
	AND EXISTS (
		SELECT 1 FROM management_grants g
		JOIN management_role_actions a ON a.role=g.role
		WHERE g.user_id=$2 AND g.organization_id=u.organization_id AND a.action=$3
		AND (
			g.scope='organization' OR (g.scope='self' AND u.id=$2) OR
			(g.scope='group' AND EXISTS (
				SELECT 1 FROM user_group_members gm WHERE gm.group_id=g.group_id AND gm.user_id=u.id
			))
		)
	)
))`

var userWriteAllowed = strings.ReplaceAll(userAllowed, "$3", "'users.state'")

func actorSession(ctx context.Context, tx pgx.Tx, value string, write bool) (actor, error) {
	var result actor
	if !validToken(value) {
		return result, errSession
	}
	if write {
		// Serialize membership and grant mutations with scoped account writes.
		if _, err := tx.Exec(ctx, "SELECT singleton FROM organizations WHERE singleton FOR NO KEY UPDATE"); err != nil {
			return result, err
		}
	}
	query := `SELECT u.id, u.organization_id, ` + adminBinding + `
		FROM browser_sessions s JOIN users u ON s.user_id=u.id
		WHERE s.token_digest=$1 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		  AND u.status='active'`
	if write {
		query += " FOR SHARE OF u, s"
	}
	err := tx.QueryRow(ctx, query, digest(value)).Scan(&result.ID, &result.Organization, &result.PlatformAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errSession
	}
	return result, err
}

func canReadUsers(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, userID string) (bool, error) {
	var can bool
	err := db.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM management_grants g JOIN management_role_actions a ON a.role=g.role
		WHERE g.user_id=$1 AND a.action='users.read'
	)`, userID).Scan(&can)
	return can, err
}

func (s Store) getUser(ctx context.Context, credentials userReadCredentials, target string) (managedUser, error) {
	var found managedUser
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		user, scope, groupID, err := credentials.authenticate(ctx, tx)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT u.id,u.username,u.status,`+adminBinding+`, (NOT $8 AND `+userWriteAllowed+`)
			FROM users u WHERE u.organization_id=$1 AND `+userAllowed+` AND u.id=$5 AND `+tokenAllowed,
			user.Organization, user.ID, "users.read", user.PlatformAdmin, target, scope, groupID, credentials.fromAPI).
			Scan(&found.ID, &found.Username, &found.Status, &found.PlatformAdmin, &found.CanChange)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUserMissing
		}
		return err
	})
	return found, err
}

var errGroupConflict = errors.New("group_name_in_use")
var errGroupMissing = errors.New("group_not_found")

type group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type managementGrant struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Scope    string `json:"scope"`
	GroupID  string `json:"group_id,omitempty"`
}

func (a *API) groupList(w http.ResponseWriter, r *http.Request) {
	result, err := readAsAdmin(r.Context(), a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]group, error) {
		rows, err := tx.Query(r.Context(), "SELECT id,name FROM user_groups WHERE organization_id=$1 ORDER BY name,id LIMIT 501", org)
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[group])
		if list == nil {
			list = []group{}
		}
		return list, err
	})
	if err != nil {
		failure(w, "list groups", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func readAsAdmin[T any](ctx context.Context, s Store, session string, read func(pgx.Tx, string) (T, error)) (T, error) {
	var value T
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		_, org, err := managementActor(ctx, tx, session, false)
		if err != nil {
			return err
		}
		value, err = read(tx, org)
		return err
	})
	return value, err
}

func (a *API) createGroup(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if !validOrganization(input.Name) {
		writeError(w, 400, "invalid_group")
		return
	}
	var result group
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		actor, org, err := managementActor(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, "INSERT INTO user_groups(organization_id,name) VALUES($1,$2) RETURNING id,name", org, input.Name).Scan(&result.ID, &result.Name)
		var dbErr *pgconn.PgError
		if errors.As(err, &dbErr) && dbErr.Code == "23505" {
			return errGroupConflict
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, org, actor, "group.created", result.ID)
	})
	if err != nil {
		failure(w, "create group", err)
		return
	}
	writeJSON(w, 201, result)
}

func userByUsername(ctx context.Context, tx pgx.Tx, org, name string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, "SELECT id FROM users WHERE organization_id=$1 AND username=$2 AND status<>'archived'", org, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errUserMissing
	}
	return id, err
}

func requireGroup(ctx context.Context, tx pgx.Tx, org, id string) error {
	var found bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM user_groups WHERE organization_id=$1 AND id=$2)", org, id).Scan(&found)
	if err == nil && !found {
		err = errGroupMissing
	}
	return err
}

func (a *API) groupMember(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "group member", errGroupMissing)
		return
	}
	var input struct {
		Username string `json:"username"`
		Remove   bool   `json:"remove"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		actor, org, err := managementActor(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		if err := requireGroup(ctx, tx, org, id); err != nil {
			return err
		}
		user, err := userByUsername(ctx, tx, org, normalizeUsername(input.Username))
		if err != nil {
			return err
		}
		query, action := "INSERT INTO user_group_members(organization_id,group_id,user_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", "group.member_added"
		if input.Remove {
			query, action = "DELETE FROM user_group_members WHERE organization_id=$1 AND group_id=$2 AND user_id=$3", "group.member_removed"
		}
		tag, err := tx.Exec(ctx, query, org, id, user)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return auditWithReason(ctx, tx, org, actor, action, id, user)
	})
	if err != nil {
		failure(w, "change group member", err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) groupMembers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "group members", errGroupMissing)
		return
	}
	result, err := readAsAdmin(r.Context(), a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]string, error) {
		if err := requireGroup(r.Context(), tx, org, id); err != nil {
			return nil, err
		}
		rows, err := tx.Query(r.Context(), `SELECT u.username FROM user_group_members m JOIN users u ON u.id=m.user_id
			WHERE m.organization_id=$1 AND m.group_id=$2 ORDER BY u.username LIMIT 501`, org, id)
		if err != nil {
			return nil, err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if names == nil {
			names = []string{}
		}
		return names, err
	})
	if err != nil {
		failure(w, "list group members", err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) grantList(w http.ResponseWriter, r *http.Request) {
	result, err := readAsAdmin(r.Context(), a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]managementGrant, error) {
		rows, err := tx.Query(r.Context(), `SELECT g.id,u.username,g.role,g.scope,COALESCE(g.group_id::text,'')
			FROM management_grants g JOIN users u ON u.id=g.user_id WHERE g.organization_id=$1
			ORDER BY u.username,g.role,g.id LIMIT 501`, org)
		if err != nil {
			return nil, err
		}
		grants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[managementGrant])
		if grants == nil {
			grants = []managementGrant{}
		}
		return grants, err
	})
	if err != nil {
		failure(w, "list grants", err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) grantRole(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var input managementGrant
	if !readJSON(w, r, &input) {
		return
	}
	if input.ID != "" || (input.Role != "user_viewer" && input.Role != "user_manager") ||
		(input.Scope != "organization" && input.Scope != "group" && input.Scope != "self") ||
		(input.Scope == "group" && !userIDPattern.MatchString(input.GroupID)) ||
		(input.Scope != "group" && input.GroupID != "") {
		writeError(w, 400, "invalid_grant")
		return
	}
	input.Username = normalizeUsername(input.Username)
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		actor, org, err := managementActor(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		user, err := userByUsername(ctx, tx, org, input.Username)
		if err != nil {
			return err
		}
		if input.GroupID != "" {
			if err := requireGroup(ctx, tx, org, input.GroupID); err != nil {
				return err
			}
		}
		err = tx.QueryRow(ctx, `INSERT INTO management_grants(organization_id,user_id,role,scope,group_id)
			VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid) ON CONFLICT DO NOTHING RETURNING id`,
			org, user, input.Role, input.Scope, input.GroupID).Scan(&input.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `SELECT id FROM management_grants WHERE user_id=$1 AND role=$2 AND scope=$3
				AND group_id IS NOT DISTINCT FROM NULLIF($4,'')::uuid`, user, input.Role, input.Scope, input.GroupID).Scan(&input.ID)
		}
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, org, actor, "authorization.granted", input.ID, user)
	})
	if err != nil {
		failure(w, "grant role", err)
		return
	}
	writeJSON(w, 201, input)
}

func (a *API) revokeRole(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		writeError(w, 404, "not_found")
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		actor, org, err := managementActor(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "DELETE FROM management_grants WHERE id=$1 AND organization_id=$2", id, org)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return audit(ctx, tx, org, actor, "authorization.revoked", id)
	})
	if err != nil {
		failure(w, "revoke role", err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) getUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "read user", errUserMissing)
		return
	}
	row, err := a.store.getUser(r.Context(), a.readCredentials(r), id)
	if err != nil {
		failure(w, "read user", err)
		return
	}
	writeJSON(w, 200, row)
}
