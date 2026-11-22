package m1

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	errControlRoleConflict = errors.New("control_role_name_in_use")
	errControlRoleVersion  = errors.New("control_role_stale")
	errControlRoleMissing  = errors.New("control_role_not_found")
)

type controlRole struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Actions []string `json:"allowed_actions"`
	Version int64    `json:"version"`
}

func distinctControlActions(actions []string) bool {
	if len(actions) == 0 || len(actions) > 5 {
		return false
	}
	seen := make(map[string]struct{}, len(actions))
	for _, value := range actions {
		switch value {
		case "view", "input", "audio", "clipboard", "file_transfer":
		default:
			return false
		}
		if _, found := seen[value]; found {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func (a *API) listControlRoles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result, err := readAsAdmin(ctx, a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]controlRole, error) {
		rows, err := tx.Query(ctx, "SELECT id,name,allowed_actions,version FROM control_roles WHERE organization_id=$1 ORDER BY name,id", org)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByPos[controlRole])
	})
	if err != nil {
		failure(w, "list control roles", err)
		return
	}
	if result == nil {
		result = []controlRole{}
	}
	writeJSON(w, 200, result)
}

func (a *API) saveControlRole(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	var input controlRole
	if !readJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.ID != "" || !validOrganization(input.Name) || !distinctControlActions(input.Actions) {
		writeError(w, 400, "invalid_control_role")
		return
	}
	if (id != "" && (!userIDPattern.MatchString(id) || input.Version < 1)) || (id == "" && input.Version != 0) {
		writeError(w, 400, "invalid_control_role")
		return
	}
	slices.Sort(input.Actions)
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		actor, org, err := managementActor(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		action := "control_role.created"
		if id == "" {
			err = tx.QueryRow(ctx, `INSERT INTO control_roles(organization_id,name,allowed_actions)
				VALUES($1,$2,$3) RETURNING id,version`, org, input.Name, input.Actions).Scan(&input.ID, &input.Version)
		} else {
			action = "control_role.updated"
			var current int64
			if err := tx.QueryRow(ctx, "SELECT version FROM control_roles WHERE id=$1 AND organization_id=$2 FOR NO KEY UPDATE", id, org).Scan(&current); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errControlRoleMissing
				}
				return err
			}
			if current != input.Version {
				return errControlRoleVersion
			}
			err = tx.QueryRow(ctx, `UPDATE control_roles SET name=$2,allowed_actions=$3,version=version+1
				WHERE id=$1 RETURNING id,version`, id, input.Name, input.Actions).Scan(&input.ID, &input.Version)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return errControlRoleConflict
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, org, actor, action, input.ID)
	})
	if err != nil {
		failure(w, "save control role", err)
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, input)
}
