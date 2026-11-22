package m1

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

var (
	errTOTPCode        = errors.New("invalid_verification_code")
	errTOTPEnrollment  = errors.New("totp_enrollment_unavailable")
	errTOTPUnavailable = errors.New("totp_unavailable")
)

type loginResult struct {
	value     string
	challenge bool
}

type TOTPKey struct{ aead cipher.AEAD }

func ParseTOTPKey(value string) (*TOTPKey, error) {
	if value == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != value {
		return nil, errors.New("DESKREALM_TOTP_KEY must be a Base64 encoded 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &TOTPKey{aead}, nil
}

func (key *TOTPKey) encrypt(userID, secret string) []byte {
	return key.aead.Seal([]byte{1}, nil, []byte(secret), []byte("deskrealm/totp/v1/"+userID))
}

func (key *TOTPKey) decrypt(userID string, data []byte) (string, error) {
	if key == nil || len(data) < 2 || data[0] != 1 {
		return "", errTOTPUnavailable
	}
	body, err := key.aead.Open(nil, nil, data[1:], []byte("deskrealm/totp/v1/"+userID))
	if err != nil {
		return "", errTOTPUnavailable
	}
	return string(body), nil
}

func (s Store) beginLogin(ctx context.Context, tx pgx.Tx, userID, orgID, previous string) (loginResult, error) {
	value, err := token()
	if err != nil {
		return loginResult{}, err
	}
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_totp WHERE user_id=$1 AND enabled)`, userID).Scan(&enabled); err != nil {
		return loginResult{}, err
	}
	if enabled {
		if _, err := tx.Exec(ctx, `INSERT INTO totp_challenges (digest,user_id,expires_at)
			VALUES ($1,$2,clock_timestamp()+interval '5 minutes')
			ON CONFLICT (user_id) DO UPDATE SET digest=EXCLUDED.digest, expires_at=EXCLUDED.expires_at, failures=0`, digest(value), userID); err != nil {
			return loginResult{}, err
		}
		return loginResult{value, true}, nil
	}
	return loginResult{value, false}, createSession(ctx, tx, userID, orgID, previous, value)
}

func (s Store) enrollTOTP(ctx context.Context, key *TOTPKey, actorToken string, user principal, password string) (string, error) {
	secretBytes := make([]byte, 20)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", err
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secretBytes)
	err := s.reauthenticate(ctx, actorToken, user.UserID, password, func(tx pgx.Tx, orgID string) error {
		tag, err := tx.Exec(ctx, `INSERT INTO user_totp (user_id,ciphertext) VALUES ($1,$2)
			ON CONFLICT (user_id) DO UPDATE SET ciphertext=EXCLUDED.ciphertext WHERE NOT user_totp.enabled`,
			user.UserID, key.encrypt(user.UserID, secret))
		if err == nil && tag.RowsAffected() != 1 {
			return errTOTPEnrollment
		}
		return err
	})
	return secret, err
}

func (s Store) reauthenticate(ctx context.Context, sessionToken, userID, password string, action func(pgx.Tx, string) error) error {
	var hash string
	err := s.DB.QueryRow(ctx, "SELECT COALESCE(password_hash,'') FROM users WHERE id=$1 AND status='active'", userID).Scan(&hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !passwordMatches(password, hash) {
		return errCredentials
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var orgID, current string
		err := tx.QueryRow(ctx, `SELECT organization_id,COALESCE(password_hash,'') FROM users
			WHERE id=$1 AND status='active' FOR NO KEY UPDATE`, userID).Scan(&orgID, &current)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && current != hash) {
			return errCredentials
		}
		if err != nil {
			return err
		}
		var ok bool
		err = tx.QueryRow(ctx, `SELECT true FROM browser_sessions WHERE token_digest=$1
			AND user_id=$2 AND revoked_at IS NULL AND expires_at>clock_timestamp() FOR SHARE`, digest(sessionToken), userID).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSession
		}
		if err != nil {
			return err
		}
		if !ok {
			return errSession
		}
		return action(tx, orgID)
	})
}

func totpStep(secret, code string, now time.Time, last int64) (int64, bool) {
	current := now.Unix() / 30
	for _, step := range []int64{current, current - 1, current + 1} {
		if step <= last {
			continue
		}
		valid, err := totp.ValidateCustom(code, secret, time.Unix(step*30, 0), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix})
		if err == nil && valid {
			return step, true
		}
	}
	return 0, false
}

func (s Store) confirmTOTP(ctx context.Context, key *TOTPKey, actorToken string, user principal, password, code string) error {
	return s.reauthenticate(ctx, actorToken, user.UserID, password, func(tx pgx.Tx, orgID string) error {
		var sealed []byte
		err := tx.QueryRow(ctx, "SELECT ciphertext FROM user_totp WHERE user_id=$1 AND NOT enabled FOR UPDATE", user.UserID).Scan(&sealed)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTOTPEnrollment
		}
		if err != nil {
			return err
		}
		secret, err := key.decrypt(user.UserID, sealed)
		if err != nil {
			return err
		}
		step, ok := totpStep(secret, code, time.Now(), -1)
		if !ok {
			return errTOTPCode
		}
		if _, err := tx.Exec(ctx, "UPDATE user_totp SET enabled=true,last_step=$2 WHERE user_id=$1", user.UserID, step); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE browser_sessions SET revoked_at=now()
			WHERE user_id=$1 AND token_digest<>$2 AND revoked_at IS NULL`, user.UserID, digest(actorToken)); err != nil {
			return err
		}
		return audit(ctx, tx, orgID, user.UserID, "user.totp_enabled", user.UserID)
	})
}

func (s Store) completeTOTP(ctx context.Context, key *TOTPKey, challenge, code, previous string) (string, error) {
	value, err := token()
	if err != nil {
		return "", err
	}
	var accepted bool
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var userID, orgID string
		err := tx.QueryRow(ctx, `SELECT u.id,u.organization_id FROM users u JOIN totp_challenges c ON c.user_id=u.id
			WHERE c.digest=$1 AND u.status='active' FOR NO KEY UPDATE OF u`, digest(challenge)).Scan(&userID, &orgID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTOTPCode
		}
		if err != nil {
			return err
		}
		var sealed []byte
		var last int64
		err = tx.QueryRow(ctx, `SELECT a.ciphertext,a.last_step FROM user_totp a JOIN totp_challenges c ON c.user_id=a.user_id
			WHERE a.user_id=$1 AND a.enabled AND c.digest=$2 AND c.failures<5 AND c.expires_at>clock_timestamp()
			FOR UPDATE OF a,c`, userID, digest(challenge)).Scan(&sealed, &last)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTOTPCode
		}
		if err != nil {
			return err
		}
		secret, err := key.decrypt(userID, sealed)
		if err != nil {
			return err
		}
		step, matches := totpStep(secret, code, time.Now(), last)
		if !matches {
			_, err := tx.Exec(ctx, "UPDATE totp_challenges SET failures=failures+1 WHERE digest=$1", digest(challenge))
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE user_totp SET last_step=$2 WHERE user_id=$1", userID, step); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM totp_challenges WHERE digest=$1", digest(challenge)); err != nil {
			return err
		}
		accepted = true
		return createSession(ctx, tx, userID, orgID, previous, value)
	})
	if err == nil && !accepted {
		err = errTOTPCode
	}
	return value, err
}

func (a *API) loginResponse(w http.ResponseWriter, result loginResult) {
	if result.challenge {
		if a.totp == nil {
			failure(w, "login verification", errTOTPUnavailable)
			return
		}
		a.transientCookie(w, "totp", result.value, 300)
		writeJSON(w, http.StatusAccepted, map[string]string{"next": "totp"})
		return
	}
	a.setCookie(w, result.value, int(sessionLifetime.Seconds()))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) transientName(purpose string) string {
	if a.secure {
		return "__Host-deskrealm-" + purpose
	}
	return "deskrealm-local-" + purpose
}

func (a *API) transientCookie(w http.ResponseWriter, purpose, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: a.transientName(purpose), Value: value, Path: "/", Secure: a.secure,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func (a *API) transientValue(r *http.Request, purpose string) string {
	if c, err := r.Cookie(a.transientName(purpose)); err == nil {
		return c.Value
	}
	return ""
}

func (a *API) verifyLoginTOTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	challenge := a.transientValue(r, "totp")
	if a.totp == nil || !validToken(challenge) || len(input.Code) != 6 {
		failure(w, "login verification", errTOTPCode)
		return
	}
	value, err := a.store.completeTOTP(r.Context(), a.totp, challenge, input.Code, a.cookieValue(r))
	if err != nil {
		failure(w, "login verification", err)
		return
	}
	a.transientCookie(w, "totp", "", -1)
	a.loginResponse(w, loginResult{value, false})
}

func (a *API) totpStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	var enabled bool
	if err := a.store.DB.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM user_totp WHERE user_id=$1 AND enabled)`, user.UserID).Scan(&enabled); err != nil {
		failure(w, "verification status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"available": a.totp != nil, "enabled": enabled})
}

func (a *API) changeTOTP(w http.ResponseWriter, r *http.Request) {
	user, ok := a.sessionWrite(w, r)
	if !ok {
		return
	}
	if a.totp == nil {
		failure(w, "verification configuration", errTOTPUnavailable)
		return
	}
	var input struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if len(input.Password) == 0 || len(input.Password) > 256 {
		failure(w, "verification configuration", errCredentials)
		return
	}
	if !a.admit(r) {
		writeError(w, http.StatusTooManyRequests, "try_again_later")
		return
	}
	defer func() { <-a.hashSlots }()
	if r.URL.Path == "/v1/auth/totp/setup" {
		secret, err := a.store.enrollTOTP(r.Context(), a.totp, a.cookieValue(r), user, input.Password)
		if err != nil {
			failure(w, "configure verification", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"secret": secret})
		return
	}
	if err := a.store.confirmTOTP(r.Context(), a.totp, a.cookieValue(r), user, input.Password, input.Code); err != nil {
		failure(w, "enable verification", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
