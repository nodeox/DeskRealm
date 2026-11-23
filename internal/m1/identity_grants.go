package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	invitationGrant    = "invitation"
	recoveryGrant      = "recovery"
	invitationLifetime = time.Hour
	recoveryLifetime   = 15 * time.Minute
)

var (
	errIdentityGrant  = errors.New("invalid_or_expired_identity_link")
	errImportConflict = errors.New("import_request_conflict")
	importKeyPattern  = regexp.MustCompile(`^[a-zA-Z0-9_-]{12,64}$`)
)

type issuedGrant struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s Store) issueIdentityGrant(ctx context.Context, actorToken, kind, subject string) (issuedGrant, error) {
	var result issuedGrant
	value, err := token()
	if err != nil {
		return result, err
	}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		actorID, orgID, err := managementActor(ctx, tx, actorToken, true)
		if err != nil {
			return err
		}
		var userID, status string
		lifetime := recoveryLifetime
		if kind == invitationGrant {
			lifetime = invitationLifetime
			err = tx.QueryRow(ctx, `SELECT id,status FROM users WHERE organization_id=$1 AND username=$2 FOR NO KEY UPDATE`, orgID, subject).
				Scan(&userID, &status)
			if errors.Is(err, pgx.ErrNoRows) {
				status = "pending"
				err = tx.QueryRow(ctx, `INSERT INTO users (organization_id,username,status)
					VALUES ($1,$2,'pending') RETURNING id`, orgID, subject).Scan(&userID)
			}
			if err == nil && status != "pending" {
				return errUserExists
			}
		} else {
			err = tx.QueryRow(ctx, `SELECT id,status FROM users WHERE organization_id=$1 AND id=$2 FOR NO KEY UPDATE`, orgID, subject).
				Scan(&userID, &status)
			if errors.Is(err, pgx.ErrNoRows) {
				return errUserMissing
			}
			if err == nil && status == "pending" {
				return errUserTransition
			}
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_grants SET consumed_at=now()
			WHERE user_id=$1 AND kind=$2 AND consumed_at IS NULL`, userID, kind); err != nil {
			return err
		}
		result.ExpiresAt = time.Now().UTC().Add(lifetime)
		if _, err := tx.Exec(ctx, `INSERT INTO identity_grants (token_digest,organization_id,user_id,created_by,kind,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6)`, digest(value), orgID, userID, actorID, kind, result.ExpiresAt); err != nil {
			return err
		}
		result.URL = value
		return audit(ctx, tx, orgID, actorID, "user."+kind+"_issued", userID)
	})
	return result, err
}

func (s Store) redeemIdentityGrant(ctx context.Context, kind, value, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	key := digest(value)
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := lockSessionAuthorization(ctx, tx); err != nil {
			return err
		}
		var orgID, userID string
		err := tx.QueryRow(ctx, `SELECT organization_id,user_id FROM identity_grants
			WHERE token_digest=$1 AND kind=$2 AND consumed_at IS NULL AND expires_at>clock_timestamp()`, key, kind).
			Scan(&orgID, &userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errIdentityGrant
		}
		if err != nil {
			return err
		}
		var status string
		err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 AND organization_id=$2 FOR NO KEY UPDATE`, userID, orgID).Scan(&status)
		if err != nil {
			return err
		}
		if kind == invitationGrant {
			if status != "pending" {
				return errIdentityGrant
			}
			status = "active"
		} else {
			if status == "pending" {
				return errIdentityGrant
			}
			if status == "archived" {
				status = "disabled"
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE identity_grants SET consumed_at=now()
			WHERE token_digest=$1 AND consumed_at IS NULL AND expires_at>clock_timestamp()`, key)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errIdentityGrant
		}
		if _, err := tx.Exec(ctx, "UPDATE users SET status=$2,password_hash=$3 WHERE id=$1", userID, status, hash); err != nil {
			return err
		}
		if err := revokeUserLogins(ctx, tx, userID); err != nil {
			return err
		}
		return audit(ctx, tx, orgID, userID, "user."+kind+"_redeemed", userID)
	})
}

type importEntry struct {
	Username string `json:"username"`
	Result   string `json:"result"`
}

type importResult struct {
	ID      string        `json:"id"`
	Entries []importEntry `json:"entries"`
}

func (s Store) importUsers(ctx context.Context, actorToken, requestKey string, usernames []string) (importResult, error) {
	var output importResult
	payload, err := json.Marshal(usernames)
	if err != nil {
		return output, err
	}
	hash := digest(string(payload))
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		actorID, orgID, err := managementActor(ctx, tx, actorToken, true)
		if err != nil {
			return err
		}
		var priorDigest, priorResult []byte
		err = tx.QueryRow(ctx, `SELECT id,payload_digest,result FROM identity_imports
			WHERE organization_id=$1 AND idempotency_key=$2`, orgID, requestKey).
			Scan(&output.ID, &priorDigest, &priorResult)
		switch {
		case err == nil:
			if !bytes.Equal(hash, priorDigest) {
				return errImportConflict
			}
			return json.Unmarshal(priorResult, &output.Entries)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,kind,owner_id)
			VALUES($1,'user_import',$2) RETURNING id`, orgID, actorID).Scan(&output.ID); err != nil {
			return err
		}
		output.Entries = make([]importEntry, 0, len(usernames))
		for _, name := range usernames {
			result, err := importAccount(ctx, tx, orgID, actorID, output.ID, name)
			if err != nil {
				return err
			}
			output.Entries = append(output.Entries, importEntry{name, result})
			failed := result == "import_failed"
			if _, err := tx.Exec(ctx, `WITH target AS (
				INSERT INTO task_targets(task_id,object_key,state,result_code,retryable)
				VALUES($1,$2,CASE WHEN $4 THEN 'failed' ELSE 'succeeded' END,$3,$4) RETURNING id,version
			)
			INSERT INTO task_attempts(id,target_id,sequence,requested_by,result_code,final_version,finished_at)
			SELECT gen_random_uuid(),id,0,$5,$3,version,clock_timestamp() FROM target`, output.ID, name, result, failed, actorID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_imports (id,organization_id,idempotency_key,payload_digest,result,created_by)
			VALUES ($1,$2,$3,$4,$5,$6)`, output.ID, orgID, requestKey, hash, output.Entries, actorID); err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: orgID, ActorID: actorID,
			ResourceID: output.ID, TaskID: output.ID, Action: "user.import_batch", Result: "success"})
		return err
	})
	return output, err
}

func (a *API) inviteUser(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var input struct {
		Username string `json:"username"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	name := normalizeUsername(input.Username)
	if !usernamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_username")
		return
	}
	a.issueUserLink(w, r, invitationGrant, name, "/accept-invitation")
}

func (a *API) recoverUser(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, errUserMissing.Error())
		return
	}
	a.issueUserLink(w, r, recoveryGrant, id, "/recover")
}

func (a *API) issueUserLink(w http.ResponseWriter, r *http.Request, kind, subject, page string) {
	result, err := a.store.issueIdentityGrant(r.Context(), a.cookieValue(r), kind, subject)
	if err != nil {
		failure(w, "issue account link", err)
		return
	}
	result.URL = a.origin + page + "#ticket=" + result.URL
	writeJSON(w, http.StatusCreated, result)
}

func (a *API) redeemUserLink(w http.ResponseWriter, r *http.Request) {
	kind := invitationGrant
	if r.URL.Path == "/v1/auth/recover" {
		kind = recoveryGrant
	}
	var input struct {
		Ticket   string `json:"ticket"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	switch {
	case !validToken(input.Ticket):
		failure(w, "redeem account link", errIdentityGrant)
	case !validPassword(input.Password):
		writeError(w, http.StatusBadRequest, "invalid_password")
	case !a.admit(r):
		writeError(w, http.StatusTooManyRequests, "try_again_later")
	default:
		defer func() { <-a.hashSlots }()
		if err := a.store.redeemIdentityGrant(r.Context(), kind, input.Ticket, input.Password); err != nil {
			failure(w, "redeem account link", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) importUsers(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var input struct {
		Key       string   `json:"idempotency_key"`
		Usernames []string `json:"usernames"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !importKeyPattern.MatchString(input.Key) || len(input.Usernames) == 0 || len(input.Usernames) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_import")
		return
	}
	seen := make(map[string]bool, len(input.Usernames))
	for i, raw := range input.Usernames {
		name := normalizeUsername(raw)
		if !usernamePattern.MatchString(name) || seen[name] {
			writeError(w, http.StatusBadRequest, "invalid_import")
			return
		}
		seen[name] = true
		input.Usernames[i] = name
	}
	result, err := a.store.importUsers(r.Context(), a.cookieValue(r), input.Key, input.Usernames)
	if err != nil {
		failure(w, "import users", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
