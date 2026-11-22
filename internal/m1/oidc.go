package m1

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

type OIDC struct {
	issuer   string
	config   oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

func allowedIssuerURL(value string, dev bool) bool {
	u, err := url.Parse(value)
	return err == nil && u.Hostname() != "" && u.User == nil && u.Fragment == "" &&
		(u.Scheme == "https" || (dev && u.Scheme == "http" && Loopback(u.Hostname())))
}

func ValidateIssuer(issuer string, dev bool) error {
	if !allowedIssuerURL(issuer, dev) || strings.ContainsAny(issuer, "?#") {
		return errors.New("OIDC issuer must be an HTTPS URL or a development loopback URL")
	}
	return nil
}

func OpenOIDC(ctx context.Context, issuer, id, secret, origin string, dev bool) (*OIDC, error) {
	if issuer == "" && id == "" && secret == "" {
		return nil, nil
	}
	if id == "" || secret == "" {
		return nil, errors.New("OIDC client ID and secret are required when enabled")
	}
	if err := ValidateIssuer(issuer, dev); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(next *http.Request, chain []*http.Request) error {
		if len(chain) >= 5 || !allowedIssuerURL(next.URL.String(), dev) ||
			next.URL.Scheme != chain[0].URL.Scheme || next.URL.Host != chain[0].URL.Host {
			return errors.New("unexpected OIDC endpoint redirect")
		}
		return nil
	}}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	endpoint := provider.Endpoint()
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil ||
		!allowedIssuerURL(endpoint.AuthURL, dev) || !allowedIssuerURL(endpoint.TokenURL, dev) ||
		!allowedIssuerURL(metadata.JWKS, dev) {
		return nil, errors.New("invalid OIDC provider endpoints")
	}
	return &OIDC{
		issuer: issuer, client: client,
		config: oauth2.Config{ClientID: id, ClientSecret: secret, Endpoint: endpoint,
			RedirectURL: origin + "/v1/auth/oidc/callback", Scopes: []string{oidc.ScopeOpenID}},
		verifier: provider.VerifierContext(oidc.ClientContext(context.Background(), client), &oidc.Config{ClientID: id}),
	}, nil
}

func (s Store) BindOIDC(ctx context.Context, issuer, subject, username string) error {
	username = normalizeUsername(username)
	if !usernamePattern.MatchString(username) || subject == "" || len(subject) > 255 || strings.ContainsRune(subject, 0) {
		return errors.New("valid OIDC subject and local username required")
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var userID, orgID string
		err := tx.QueryRow(ctx, `SELECT id,organization_id FROM users WHERE username=$1 AND status='active' FOR NO KEY UPDATE`, username).
			Scan(&userID, &orgID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUserMissing
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "INSERT INTO oidc_identities (issuer,subject,user_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING", issuer, subject, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var bound string
			err := tx.QueryRow(ctx, "SELECT user_id FROM oidc_identities WHERE issuer=$1 AND subject=$2", issuer, subject).Scan(&bound)
			if err == nil && bound == userID {
				return nil
			}
			return errors.New("OIDC identity or account already has a different binding")
		}
		return auditWithReason(ctx, tx, orgID, "", "user.oidc_bound", userID, "center console")
	})
}

type oidcAttempt struct{ state, browser, verifier, nonce string }

func (s Store) createOIDCAttempt(ctx context.Context) (oidcAttempt, error) {
	values := [3]string{}
	for i := range values {
		value, err := token()
		if err != nil {
			return oidcAttempt{}, err
		}
		values[i] = value
	}
	attempt := oidcAttempt{values[0], values[1], oauth2.GenerateVerifier(), values[2]}
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM oidc_logins WHERE expires_at <= now()"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO oidc_logins (digest,browser_digest,verifier,nonce,expires_at)
			VALUES ($1,$2,$3,$4,clock_timestamp()+interval '5 minutes')`, digest(attempt.state), digest(attempt.browser),
			attempt.verifier, attempt.nonce)
		return err
	})
	return attempt, err
}

func (a *API) authOptions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"oidc": a.oidc != nil})
}

func (a *API) startOIDC(w http.ResponseWriter, r *http.Request) {
	if a.oidc == nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if !a.admit(r) {
		writeError(w, http.StatusTooManyRequests, "try_again_later")
		return
	}
	defer func() { <-a.hashSlots }()
	attempt, err := a.store.createOIDCAttempt(r.Context())
	if err != nil {
		failure(w, "start external login", err)
		return
	}
	a.transientCookie(w, "oidc", attempt.browser, 300)
	location := a.oidc.config.AuthCodeURL(attempt.state, oidc.Nonce(attempt.nonce), oauth2.S256ChallengeOption(attempt.verifier))
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (a *API) callbackOIDC(w http.ResponseWriter, r *http.Request) {
	if a.oidc == nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	fail := func() { http.Redirect(w, r, "/login?oidc=failed", http.StatusSeeOther) }
	query := r.URL.Query()
	state, browser := query.Get("state"), a.transientValue(r, "oidc")
	if len(query["state"]) != 1 || !validToken(state) || !validToken(browser) {
		fail()
		return
	}
	var verifier, nonce string
	ctx := oidc.ClientContext(r.Context(), a.oidc.client)
	err := a.store.DB.QueryRow(ctx, `DELETE FROM oidc_logins WHERE digest=$1 AND browser_digest=$2
		AND expires_at>clock_timestamp() RETURNING verifier,nonce`, digest(state), digest(browser)).Scan(&verifier, &nonce)
	if err != nil {
		fail()
		return
	}
	a.transientCookie(w, "oidc", "", -1)
	code := query.Get("code")
	if code == "" || len(code) > 4096 || len(query["code"]) != 1 || query.Has("error") {
		fail()
		return
	}
	result, err := a.oidc.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		fail()
		return
	}
	raw, ok := result.Extra("id_token").(string)
	if !ok {
		fail()
		return
	}
	id, err := a.oidc.verifier.Verify(ctx, raw)
	if err != nil || id.Nonce != nonce || id.Subject == "" || len(id.Subject) > 255 {
		fail()
		return
	}
	if len(id.Audience) > 1 {
		var claims struct {
			AuthorizedParty string `json:"azp"`
		}
		if id.Claims(&claims) != nil || claims.AuthorizedParty != a.oidc.config.ClientID {
			fail()
			return
		}
	}
	var login loginResult
	err = pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		var userID, orgID string
		err := tx.QueryRow(ctx, `SELECT u.id,u.organization_id FROM oidc_identities i JOIN users u ON u.id=i.user_id
			WHERE issuer=$1 AND subject=$2 AND u.status='active' FOR NO KEY UPDATE OF u`, a.oidc.issuer, id.Subject).Scan(&userID, &orgID)
		if err != nil {
			return err
		}
		login, err = a.store.beginLogin(ctx, tx, userID, orgID, a.cookieValue(r))
		return err
	})
	if err != nil {
		fail()
		return
	}
	if login.challenge {
		if a.totp == nil {
			fail()
			return
		}
		a.transientCookie(w, "totp", login.value, 300)
		http.Redirect(w, r, "/login?verification=totp", http.StatusSeeOther)
		return
	}
	a.setCookie(w, login.value, int(sessionLifetime.Seconds()))
	http.Redirect(w, r, "/overview", http.StatusSeeOther)
}
