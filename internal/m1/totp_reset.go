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
	_, err := tx.Exec(ctx, "UPDATE browser_sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL", id)
	return err
}

func (s Store) resetUserTOTP(ctx context.Context, session, target, password, reason string) error {
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
		var currentHash, userID string
		if err := tx.QueryRow(ctx, "SELECT COALESCE(password_hash,'') FROM users WHERE id=$1", actor).Scan(&currentHash); err != nil {
			return err
		}
		if hash != currentHash {
			return errCredentials
		}
		err = tx.QueryRow(ctx, "SELECT id FROM users WHERE id=$1 AND organization_id=$2 FOR NO KEY UPDATE", target, org).Scan(&userID)
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
	var in struct {
		Password string `json:"password"`
		Reason   string `json:"reason"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || utf8.RuneCountInString(in.Reason) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_reason")
		return
	}
	if len(in.Password) == 0 || len(in.Password) > 256 {
		failure(w, "reset verification", errCredentials)
		return
	}
	if !a.admit(r) {
		writeError(w, http.StatusTooManyRequests, "try_again_later")
		return
	}
	defer func() { <-a.hashSlots }()
	if err := a.store.resetUserTOTP(r.Context(), a.cookieValue(r), id, in.Password, in.Reason); err != nil {
		failure(w, "reset verification", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
