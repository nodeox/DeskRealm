package m1

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

var errAdminTransfer = errors.New("invalid_admin_transfer")

func (s Store) transferPlatformAdmin(ctx context.Context, session, target, password, reason string) error {
	return s.confirmAdminPassword(ctx, session, password, func(tx pgx.Tx, actor, org string) error {
		if actor == target {
			return errAdminTransfer
		}
		var eligible bool
		err := tx.QueryRow(ctx, `SELECT status='active' AND password_hash IS NOT NULL
			FROM users WHERE id=$1 AND organization_id=$2 FOR NO KEY UPDATE`, target, org).Scan(&eligible)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !eligible) {
			return errAdminTransfer
		}
		if err != nil {
			return err
		}
		var roleID string
		if err := tx.QueryRow(ctx, "SELECT id FROM roles WHERE organization_id=$1 AND name=$2", org, adminRole).Scan(&roleID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(organization_id,user_id,role_id)
			VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, org, target, roleID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM role_bindings WHERE user_id=$1 AND role_id=$2", actor, roleID); err != nil {
			return err
		}
		for _, id := range []string{actor, target} {
			if err := revokeUserLogins(ctx, tx, id); err != nil {
				return err
			}
		}
		return auditWithReason(ctx, tx, org, actor, "authorization.platform_admin_transferred", target, reason)
	})
}

func (a *API) transferAdmin(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "transfer admin role", errUserMissing)
		return
	}
	in, ok := a.recoveryInput(w, r)
	if !ok {
		return
	}
	defer func() { <-a.hashSlots }()
	if err := a.store.transferPlatformAdmin(r.Context(), a.cookieValue(r), id, in.Password, in.Reason); err != nil {
		failure(w, "transfer admin role", err)
		return
	}
	a.setCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
