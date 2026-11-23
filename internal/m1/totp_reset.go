package m1

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

func revokeUserLogins(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, "DELETE FROM totp_challenges WHERE user_id=$1", id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE api_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL", id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE browser_sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL", id); err != nil {
		return err
	}
	return recheckSessions(ctx, tx, "user_id=$1", id)
}

func (s Store) confirmAdminPassword(ctx context.Context, session, password string, action func(pgx.Tx, string, string) error) error {
	var hash string
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(u.password_hash,'') FROM browser_sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_digest=$1 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp() AND u.status='active'`,
		digest(session)).Scan(&hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !passwordMatches(password, hash) {
		return errCredentials
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		actor, org, err := managementActor(ctx, tx, session, true)
		if err != nil {
			return err
		}
		var currentHash string
		if err := tx.QueryRow(ctx, "SELECT COALESCE(password_hash,'') FROM users WHERE id=$1", actor).Scan(&currentHash); err != nil {
			return err
		}
		if hash != currentHash {
			return errCredentials
		}
		return action(tx, actor, org)
	})
}

func (s Store) resetUserTOTP(ctx context.Context, session, target, password, reason string) error {
	return s.confirmAdminPassword(ctx, session, password, func(tx pgx.Tx, actor, org string) error {
		var userID string
		err := tx.QueryRow(ctx, "SELECT id FROM users WHERE id=$1 AND organization_id=$2 FOR NO KEY UPDATE", target, org).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUserMissing
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "DELETE FROM user_totp WHERE user_id=$1", userID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if err := revokeUserLogins(ctx, tx, userID); err != nil {
			return err
		}
		return auditWithReason(ctx, tx, org, actor, "user.totp_reset", userID, reason)
	})
}

func (a *API) resetTOTP(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "reset verification", errUserMissing)
		return
	}
	in, ok := a.recoveryInput(w, r)
	if !ok {
		return
	}
	defer func() { <-a.hashSlots }()
	if err := a.store.resetUserTOTP(r.Context(), a.cookieValue(r), id, in.Password, in.Reason); err != nil {
		failure(w, "reset verification", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type passwordConfirmation struct {
	Password string `json:"password"`
	Reason   string `json:"reason"`
}

func (a *API) recoveryInput(w http.ResponseWriter, r *http.Request) (passwordConfirmation, bool) {
	var in passwordConfirmation
	if !readJSON(w, r, &in) {
		return in, false
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || utf8.RuneCountInString(in.Reason) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_reason")
		return in, false
	}
	if len(in.Password) == 0 || len(in.Password) > 256 {
		failure(w, "reset verification", errCredentials)
		return in, false
	}
	if !a.admit(r) {
		writeError(w, http.StatusTooManyRequests, "try_again_later")
		return in, false
	}
	return in, true
}
