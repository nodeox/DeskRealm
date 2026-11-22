package m1

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	userIDPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	errForbidden      = errors.New("forbidden")
	errUserMissing    = errors.New("user_not_found")
	errUserExists     = errors.New("username_in_use")
	errLastAdmin      = errors.New("last_platform_admin")
	errUserTransition = errors.New("invalid_user_transition")
)

type managedUser struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Status        string `json:"status"`
	PlatformAdmin bool   `json:"platform_admin"`
}

type userList struct {
	Users []managedUser `json:"users"`
	Next  string        `json:"next,omitempty"`
}

const adminBinding = `EXISTS (
	SELECT 1 FROM role_bindings b JOIN roles r ON r.id=b.role_id
	WHERE b.user_id=u.id AND r.name='platform_admin'
)`

func managementActor(ctx context.Context, tx pgx.Tx, value string, write bool) (string, string, error) {
	if !validToken(value) {
		return "", "", errSession
	}
	if write {
		// Serialize changes that can remove the final administrator. Key-share operations
		// for audit and other foreign keys can still run while this lock is held.
		if _, err := tx.Exec(ctx, "SELECT singleton FROM organizations WHERE singleton FOR NO KEY UPDATE"); err != nil {
			return "", "", err
		}
	}
	query := `SELECT u.id, u.organization_id, ` + adminBinding + `
		FROM browser_sessions s JOIN users u ON s.user_id=u.id
		WHERE s.token_digest=$1 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		  AND u.status='active'`
	if write {
		query += " FOR SHARE OF u, s"
	}
	var actorID, orgID string
	var admin bool
	err := tx.QueryRow(ctx, query, digest(value)).Scan(&actorID, &orgID, &admin)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errSession
	}
	if err == nil && !admin {
		return "", "", errForbidden
	}
	return actorID, orgID, err
}

func (s Store) listUsers(ctx context.Context, value, after string) (userList, error) {
	result := userList{Users: make([]managedUser, 0)}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		_, orgID, err := managementActor(ctx, tx, value, false)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT u.id, u.username, u.status, `+adminBinding+`
			FROM users u WHERE u.organization_id=$1 AND u.username>$2 ORDER BY u.username LIMIT 101`, orgID, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u managedUser
			if err := rows.Scan(&u.ID, &u.Username, &u.Status, &u.PlatformAdmin); err != nil {
				return err
			}
			result.Users = append(result.Users, u)
		}
		if len(result.Users) > 100 {
			result.Users = result.Users[:100]
			result.Next = result.Users[99].Username
		}
		return rows.Err()
	})
	return result, err
}

func (s Store) createUser(ctx context.Context, value, username, password string) (managedUser, error) {
	var result managedUser
	hash, err := hashPassword(password)
	if err != nil {
		return result, err
	}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		actorID, orgID, err := managementActor(ctx, tx, value, true)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO users (organization_id, username, password_hash)
			VALUES ($1,$2,$3) RETURNING id,username,status`, orgID, username, hash).
			Scan(&result.ID, &result.Username, &result.Status)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_organization_id_username_key" {
			return errUserExists
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, orgID, actorID, "user.created", result.ID)
	})
	return result, err
}

func (s Store) changeUser(ctx context.Context, value, userID, targetStatus, reason string) (managedUser, error) {
	var result managedUser
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		actorID, orgID, err := managementActor(ctx, tx, value, true)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT u.id,u.username,u.status,`+adminBinding+`
			FROM users u WHERE u.id=$1 AND u.organization_id=$2 FOR NO KEY UPDATE`, userID, orgID).
			Scan(&result.ID, &result.Username, &result.Status, &result.PlatformAdmin)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUserMissing
		}
		if err != nil || result.Status == targetStatus {
			return err
		}
		if result.Status == "archived" || (result.Status == "pending" && targetStatus != "archived") {
			return errUserTransition
		}
		if result.PlatformAdmin && result.Status == "active" && targetStatus != "active" {
			var another bool
			err := tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM users u WHERE u.organization_id=$1 AND u.id<>$2 AND u.status='active' AND `+
				adminBinding+`)`, orgID, userID).Scan(&another)
			if err != nil {
				return err
			}
			if !another {
				return errLastAdmin
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE users SET status=$2 WHERE id=$1", userID, targetStatus); err != nil {
			return err
		}
		if targetStatus != "active" {
			if err := revokeUserLogins(ctx, tx, userID); err != nil {
				return err
			}
		}
		result.Status = targetStatus
		return auditWithReason(ctx, tx, orgID, actorID, "user."+targetStatus, userID, reason)
	})
	return result, err
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	after := r.URL.Query().Get("after")
	if after != "" && !usernamePattern.MatchString(after) {
		writeError(w, http.StatusBadRequest, "invalid_cursor")
		return
	}
	result, err := a.store.listUsers(r.Context(), a.cookieValue(r), after)
	if err != nil {
		failure(w, "list users", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) authorizeUserWrite(w http.ResponseWriter, r *http.Request) bool {
	user, ok := a.requireSession(w, r)
	if !ok {
		return false
	}
	if user.Role != adminRole {
		failure(w, "manage users", errForbidden)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(user.CSRF)) != 1 {
		writeError(w, http.StatusForbidden, "csrf_denied")
		return false
	}
	return true
}

func validPassword(value string) bool {
	return len(value) <= 256 && utf8.RuneCountInString(value) >= 12
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Username = normalizeUsername(input.Username)
	switch {
	case !usernamePattern.MatchString(input.Username):
		writeError(w, http.StatusBadRequest, "invalid_username")
	case !validPassword(input.Password):
		writeError(w, http.StatusBadRequest, "invalid_password")
	case !a.admit(r):
		writeError(w, http.StatusTooManyRequests, "try_again_later")
	default:
		defer func() { <-a.hashSlots }()
		user, err := a.store.createUser(r.Context(), a.cookieValue(r), input.Username, input.Password)
		if err != nil {
			failure(w, "create user", err)
			return
		}
		writeJSON(w, http.StatusCreated, user)
	}
}

func (a *API) changeUser(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, errUserMissing.Error())
		return
	}
	var target string
	switch r.PathValue("action") {
	case "enable":
		target = "active"
	case "disable":
		target = "disabled"
	case "archive":
		target = "archived"
	default:
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if input.Reason == "" || utf8.RuneCountInString(input.Reason) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_reason")
		return
	}
	result, err := a.store.changeUser(r.Context(), a.cookieValue(r), id, target, input.Reason)
	if err != nil {
		failure(w, "change user status", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
