package m1

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

type API struct {
	store     Store
	origin    string
	secure    bool
	web       fs.FS
	hashSlots chan struct{}
	mu        sync.Mutex
	attempts  map[string]attempt
	pruneAt   time.Time
	totp      *TOTPKey
	oidc      *OIDC
}

type attempt struct {
	count int
	until time.Time
}

func ParseOrigin(raw string, dev bool) (*url.URL, error) {
	origin, err := url.Parse(raw)
	if err != nil || origin == nil || origin.Hostname() == "" || origin.User != nil ||
		(origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.ForceQuery ||
		origin.Fragment != "" || origin.Opaque != "" {
		return nil, errors.New("DESKREALM_PUBLIC_ORIGIN must be a URL origin")
	}
	if origin.Scheme != "https" && !(dev && origin.Scheme == "http" && Loopback(origin.Hostname())) {
		return nil, errors.New("public origin must use HTTPS or explicitly enabled loopback HTTP")
	}
	origin.Path, origin.RawPath = "", ""
	return origin, nil
}

func Loopback(host string) bool {
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || (err == nil && ip.IsLoopback())
}

type AuthConfig struct {
	TOTP *TOTPKey
	OIDC *OIDC
}

func NewHandler(store Store, origin *url.URL, web fs.FS, config ...AuthConfig) http.Handler {
	api := &API{store: store, origin: origin.String(), secure: origin.Scheme == "https",
		web: web, hashSlots: make(chan struct{}, 4), attempts: make(map[string]attempt)}
	if len(config) > 0 {
		api.totp, api.oidc = config[0].TOTP, config[0].OIDC
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/bootstrap/status", api.bootstrapStatus)
	mux.HandleFunc("POST /v1/bootstrap", api.bootstrap)
	mux.HandleFunc("POST /v1/auth/login", api.login)
	mux.HandleFunc("GET /v1/auth/session", api.session)
	mux.HandleFunc("POST /v1/auth/logout", api.logout)
	mux.HandleFunc("GET /v1/overview", api.overview)
	mux.HandleFunc("GET /v1/admin/users", api.listUsers)
	mux.HandleFunc("GET /v1/admin/users/{id}", api.getUser)
	mux.HandleFunc("POST /v1/admin/users", api.createUser)
	mux.HandleFunc("POST /v1/admin/users/{id}/{action}", api.changeUser)
	mux.HandleFunc("POST /v1/admin/user-invitations", api.inviteUser)
	mux.HandleFunc("POST /v1/admin/users/imports", api.importUsers)
	mux.HandleFunc("POST /v1/admin/users/{id}/recovery", api.recoverUser)
	mux.HandleFunc("POST /v1/auth/accept-invitation", api.redeemUserLink)
	mux.HandleFunc("POST /v1/auth/recover", api.redeemUserLink)
	mux.HandleFunc("GET /v1/auth/options", api.authOptions)
	mux.HandleFunc("GET /v1/auth/oidc/start", api.startOIDC)
	mux.HandleFunc("GET /v1/auth/oidc/callback", api.callbackOIDC)
	mux.HandleFunc("GET /v1/auth/totp", api.totpStatus)
	mux.HandleFunc("POST /v1/auth/totp/setup", api.changeTOTP)
	mux.HandleFunc("POST /v1/auth/totp/confirm", api.changeTOTP)
	mux.HandleFunc("POST /v1/auth/totp/verify", api.verifyLoginTOTP)
	mux.HandleFunc("POST /v1/admin/users/{id}/totp-reset", api.resetTOTP)
	mux.HandleFunc("POST /v1/admin/users/{id}/transfer-platform-admin", api.transferAdmin)
	mux.HandleFunc("GET /v1/admin/groups", api.groupList)
	mux.HandleFunc("POST /v1/admin/groups", api.createGroup)
	mux.HandleFunc("GET /v1/admin/groups/{id}/members", api.groupMembers)
	mux.HandleFunc("POST /v1/admin/groups/{id}/members", api.groupMember)
	mux.HandleFunc("GET /v1/admin/role-grants", api.grantList)
	mux.HandleFunc("POST /v1/admin/role-grants", api.grantRole)
	mux.HandleFunc("POST /v1/admin/role-grants/{id}/revoke", api.revokeRole)
	mux.HandleFunc("GET /v1/admin/api-tokens", api.listTokens)
	mux.HandleFunc("POST /v1/admin/api-tokens", api.issueToken)
	mux.HandleFunc("POST /v1/admin/api-tokens/{id}/revoke", api.revokeToken)
	mux.HandleFunc("GET /v1/admin/control-roles", api.listControlRoles)
	mux.HandleFunc("POST /v1/admin/control-roles", api.saveControlRole)
	mux.HandleFunc("POST /v1/admin/control-roles/{id}", api.saveControlRole)
	mux.HandleFunc("GET /v1/admin/device-groups", api.listDeviceGroups)
	mux.HandleFunc("POST /v1/admin/device-groups", api.createDeviceGroup)
	mux.HandleFunc("GET /v1/admin/device-groups/{id}/grants", api.deviceGroupGrants)
	mux.HandleFunc("POST /v1/admin/device-groups/{id}/grants", api.setDeviceGroupGrant)
	mux.HandleFunc("POST /v1/admin/enrollment-credentials", api.issueEnrollment)
	mux.HandleFunc("GET /v1/admin/devices", api.listOrGetDevices)
	mux.HandleFunc("GET /v1/admin/devices/{id}", api.listOrGetDevices)
	mux.HandleFunc("GET /v1/admin/devices/{id}/history", api.deviceHistory)
	mux.HandleFunc("POST /v1/admin/devices/{id}/{action}", api.decideDevice)
	mux.HandleFunc("POST /v1/admin/devices/{id}/lifecycle/{change}", api.changeDevice)
	mux.HandleFunc("POST /v1/admin/devices/{id}/key-rotation", api.issueDeviceRotation)
	mux.HandleFunc("POST /v1/enrollment/challenge", api.enrollDevice)
	mux.HandleFunc("POST /v1/enrollment/complete", api.enrollDevice)
	mux.HandleFunc("POST /v1/enrollment/rotate", api.rotateDevice)
	mux.HandleFunc("GET /v1/address-books", api.listBooks)
	mux.HandleFunc("POST /v1/address-books", api.saveBook)
	mux.HandleFunc("GET /v1/address-books/{book}", api.readBook)
	mux.HandleFunc("POST /v1/address-books/{book}", api.saveBook)
	mux.HandleFunc("GET /v1/address-books/{book}/entries", api.readBookEntries)
	mux.HandleFunc("POST /v1/address-books/{book}/entries", api.saveEntry)
	mux.HandleFunc("GET /v1/address-books/{book}/entries/{entry}", api.readBookEntries)
	mux.HandleFunc("POST /v1/address-books/{book}/entries/{entry}", api.saveEntry)
	mux.HandleFunc("POST /v1/address-books/{book}/entries/{entry}/remove", api.removeEntry)
	mux.HandleFunc("GET /v1/address-books/{book}/shares", api.listBookGrants)
	mux.HandleFunc("POST /v1/address-books/{book}/shares", api.grantBook)
	mux.HandleFunc("POST /v1/address-books/{book}/shares/{grant}/revoke", api.revokeBook)
	mux.HandleFunc("GET /v1/admin/audit-events", api.readAudit)
	mux.HandleFunc("GET /v1/admin/audit-events/{id}", api.readAudit)
	mux.HandleFunc("POST /v1/admin/audit-events/export", api.exportAudit)
	mux.HandleFunc("GET /v1/admin/audit-options", api.auditOptions)
	mux.HandleFunc("GET /v1/admin/audit-grants", api.listAuditGrants)
	mux.HandleFunc("POST /v1/admin/audit-grants", api.saveAuditGrant)
	mux.HandleFunc("POST /v1/admin/audit-grants/{id}/revoke", api.revokeAuditGrant)
	mux.HandleFunc("POST /v1/admin/audit-retention", api.saveAuditRetention)
	mux.HandleFunc("POST /v1/admin/audit-retention/prune", api.pruneAudit)
	mux.HandleFunc("GET /v1/admin/tasks", api.listTasks)
	mux.HandleFunc("GET /v1/admin/tasks/{id}", api.getTask)
	mux.HandleFunc("POST /v1/admin/tasks/{id}/retry", api.retryTask)
	apiNotFound := func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found")
	}
	mux.HandleFunc("/v1", apiNotFound)
	mux.HandleFunc("/v1/", apiNotFound)
	mux.HandleFunc("/", api.frontend)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method == http.MethodPost && !isDeviceEnrollmentRequest(r) && r.Header.Get("Origin") != api.origin {
				writeError(w, http.StatusForbidden, "origin_denied")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *API) frontend(w http.ResponseWriter, r *http.Request) {
	if a.web == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	switch p {
	case "", "initialize", "login", "overview", "users", "groups", "devices", "address-books", "audit", "tasks", "api-tokens", "security", "accept-invitation", "recover":
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, a.web, "index.html")
	default:
		if !strings.HasPrefix(p, "assets/") || !fs.ValidPath(p) {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, a.web, p)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func failure(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, errTicket):
		writeError(w, http.StatusUnauthorized, errTicket.Error())
	case errors.Is(err, errInitialized):
		writeError(w, http.StatusConflict, errInitialized.Error())
	case errors.Is(err, errSession):
		writeError(w, http.StatusUnauthorized, errSession.Error())
	case errors.Is(err, errAPIToken):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, errAPIToken.Error())
	case errors.Is(err, errCredentials):
		writeError(w, http.StatusUnauthorized, errCredentials.Error())
	case errors.Is(err, errEnrollment), errors.Is(err, errDeviceRotation):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, errIdentityGrant):
		writeError(w, http.StatusUnauthorized, errIdentityGrant.Error())
	case errors.Is(err, errTOTPCode):
		writeError(w, http.StatusUnauthorized, errTOTPCode.Error())
	case errors.Is(err, errTOTPEnrollment):
		writeError(w, http.StatusConflict, errTOTPEnrollment.Error())
	case errors.Is(err, errTOTPUnavailable):
		writeError(w, http.StatusServiceUnavailable, errTOTPUnavailable.Error())
	case errors.Is(err, errForbidden):
		writeError(w, http.StatusForbidden, errForbidden.Error())
	case errors.Is(err, errUserMissing), errors.Is(err, errGroupMissing), errors.Is(err, errControlRoleMissing):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errUserExists), errors.Is(err, errLastAdmin), errors.Is(err, errUserTransition), errors.Is(err, errImportConflict), errors.Is(err, errGroupConflict), errors.Is(err, errAdminTransfer):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errControlRoleConflict), errors.Is(err, errControlRoleVersion):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errAuditInvalid), errors.Is(err, errTaskInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errAuditExportLimit), errors.Is(err, errAuditConflict), errors.Is(err, errTaskConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errDeviceMissing), errors.Is(err, errDeviceGroupMissing), errors.Is(err, errAuditMissing), errors.Is(err, errTaskMissing):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errBookMissing), errors.Is(err, errEntryMissing), errors.Is(err, errGrantMissing):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errBookConflict), errors.Is(err, errEntryConflict), errors.Is(err, errEntryStale),
		errors.Is(err, errEntryInactive), errors.Is(err, errBookShareOwner):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errEnrollmentConflict), errors.Is(err, errDeviceConflict), errors.Is(err, errDeviceGroupConflict), errors.Is(err, errDeviceDecision),
		errors.Is(err, errDeviceVersion), errors.Is(err, errDeviceLifecycle):
		writeError(w, http.StatusConflict, err.Error())
	default:
		// PostgreSQL errors can contain argument data. Only the operation is logged.
		slog.Error("DeskRealm operation failed", "operation", operation)
		writeError(w, http.StatusInternalServerError, "service_error")
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	body := http.MaxBytesReader(w, r.Body, 8<<10)
	defer body.Close()
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func (a *API) admit(r *http.Request) bool {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !now.Before(a.pruneAt) {
		for host, entry := range a.attempts {
			if !now.Before(entry.until) {
				delete(a.attempts, host)
			}
		}
		a.pruneAt = now.Add(time.Minute)
	}
	entry, known := a.attempts[peer]
	if !known && len(a.attempts) >= 4096 {
		return false
	}
	if !now.Before(entry.until) {
		entry = attempt{until: now.Add(time.Minute)}
	}
	if entry.count >= 30 {
		return false
	}
	select {
	case a.hashSlots <- struct{}{}:
		entry.count++
		a.attempts[peer] = entry
		return true
	default:
		return false
	}
}

func (a *API) bootstrapStatus(w http.ResponseWriter, r *http.Request) {
	value, err := a.store.Initialized(r.Context())
	if err != nil {
		failure(w, "bootstrap status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"initialized": value})
}

func (a *API) bootstrap(w http.ResponseWriter, r *http.Request) {
	var request bootstrapRequest
	if !readJSON(w, r, &request) {
		return
	}
	request.Organization = strings.TrimSpace(request.Organization)
	request.Username = normalizeUsername(request.Username)
	switch {
	case !validOrganization(request.Organization):
		writeError(w, http.StatusBadRequest, "invalid_organization")
	case !usernamePattern.MatchString(request.Username):
		writeError(w, http.StatusBadRequest, "invalid_username")
	case !validPassword(request.Password):
		writeError(w, http.StatusBadRequest, "invalid_password")
	case !validToken(request.Ticket):
		writeError(w, http.StatusUnauthorized, errTicket.Error())
	case !a.admit(r):
		writeError(w, http.StatusTooManyRequests, "try_again_later")
	default:
		defer func() { <-a.hashSlots }()
		if err := a.store.initialize(r.Context(), request); err != nil {
			failure(w, "bootstrap", err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Password) == 0 || len(in.Password) > 256 || len(in.Username) > 160 {
		failure(w, "login", errCredentials)
		return
	}
	if !a.admit(r) {
		writeError(w, http.StatusTooManyRequests, "try_again_later")
		return
	}
	defer func() { <-a.hashSlots }()
	result, err := a.store.login(r.Context(), normalizeUsername(in.Username), in.Password, a.cookieValue(r))
	if err != nil {
		failure(w, "login", err)
		return
	}
	a.loginResponse(w, result)
}

func (a *API) cookieName() string {
	if a.secure {
		return "__Host-deskrealm"
	}
	return "deskrealm-local"
}

func (a *API) cookieValue(r *http.Request) string {
	if len(r.Header.Values("Authorization")) != 0 {
		return ""
	}
	cookie, err := r.Cookie(a.cookieName())
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (a *API) setCookie(w http.ResponseWriter, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: value, Path: "/",
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: age})
}

func (a *API) requireSession(w http.ResponseWriter, r *http.Request) (principal, bool) {
	user, err := a.store.session(r.Context(), a.cookieValue(r))
	if err != nil {
		failure(w, "session", err)
	}
	return user, err == nil
}

func (a *API) session(w http.ResponseWriter, r *http.Request) {
	if user, ok := a.requireSession(w, r); ok {
		writeJSON(w, http.StatusOK, user)
	}
}

func (a *API) sessionWrite(w http.ResponseWriter, r *http.Request) (principal, bool) {
	user, ok := a.requireSession(w, r)
	if !ok {
		return user, false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(user.CSRF)) != 1 {
		writeError(w, http.StatusForbidden, "csrf_denied")
		return user, false
	}
	return user, true
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	user, ok := a.sessionWrite(w, r)
	if !ok {
		return
	}
	if err := a.store.logout(r.Context(), a.cookieValue(r), user); err != nil {
		failure(w, "logout", err)
		return
	}
	a.setCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
