package m1

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"deskrealm/internal/managedproto"

	"github.com/jackc/pgx/v5"
)

var (
	errSessionInput    = errors.New("invalid_managed_session_request")
	errSessionDenied   = errors.New("managed_session_denied")
	errSessionConflict = errors.New("managed_session_conflict")
)

type sessionIssueRequest struct {
	RequestID  string   `json:"request_id"`
	Target     string   `json:"target_device_id"`
	RustDeskID string   `json:"target_rustdesk_id"`
	Book       string   `json:"book_id"`
	Entry      string   `json:"entry_id"`
	Purpose    string   `json:"purpose"`
	Actions    []string `json:"requested_actions"`
	Mode       string   `json:"confirmation_mode"`
}

type sessionGrant struct {
	ID, Grant, Organization, User, Controller, Target       string
	ControllerRustDeskID, TargetRustDeskID                  string
	Book, Entry, Purpose, Kid, Mode, Rule                   string
	Actions                                                 []string
	ControllerDigest, BrowserDigest, RequestHash, Signature []byte
	ControllerKey, TargetKey                                []byte
	PolicyVersion, RuleVersion                              int64
	Mask                                                    int16
	Issued, Expires                                         time.Time
}

const sessionGrantSelect = `SELECT id,grant_id,organization_id,user_id,controller_device_id,target_device_id,
	controller_rustdesk_id,target_rustdesk_id,COALESCE(book_id::text,''),COALESCE(entry_id::text,''),purpose,signing_kid,
	confirmation_mode,rule_id,requested_actions,controller_digest,request_hash,signature,controller_key,target_key,
	policy_version,rule_version,allowed_mask,issued_at,expires_at,browser_digest FROM managed_sessions `

func readSessionGrant(row pgx.Row) (sessionGrant, error) {
	var g sessionGrant
	err := row.Scan(&g.ID, &g.Grant, &g.Organization, &g.User, &g.Controller, &g.Target, &g.ControllerRustDeskID,
		&g.TargetRustDeskID, &g.Book, &g.Entry, &g.Purpose, &g.Kid, &g.Mode, &g.Rule, &g.Actions,
		&g.ControllerDigest, &g.RequestHash, &g.Signature, &g.ControllerKey, &g.TargetKey,
		&g.PolicyVersion, &g.RuleVersion, &g.Mask, &g.Issued, &g.Expires, &g.BrowserDigest)
	return g, err
}

func (g sessionGrant) signingFrame() ([]byte, error) {
	controller, target := sha256.Sum256(g.ControllerKey), sha256.Sum256(g.TargetKey)
	mode := byte(1)
	if g.Mode == "unattended" {
		mode = 2
	}
	return managedproto.Encode(managedproto.SessionGrant, []byte(g.Kid), managedFrameUUID(g.Organization),
		managedFrameUUID(g.ID), managedFrameUUID(g.Grant), managedFrameUUID(g.User), managedFrameUUID(g.Controller),
		managedFrameUUID(g.Target), controller[:], target[:], binary.BigEndian.AppendUint64(nil, uint64(g.PolicyVersion)),
		[]byte{byte(g.Mask)}, []byte{mode}, []byte(g.Purpose), binary.BigEndian.AppendUint64(nil, uint64(g.Issued.UnixMilli())),
		binary.BigEndian.AppendUint64(nil, uint64(g.Expires.UnixMilli())))
}

func (g sessionGrant) matchesSignature(key *ManagedSigningKey, grant string, sig []byte) (bool, error) {
	if key == nil || g.Kid != key.id || g.Grant != grant || !bytes.Equal(g.Signature, sig) {
		return false, nil
	}
	frame, err := g.signingFrame()
	if err != nil {
		return false, err
	}
	return ed25519.Verify(key.key.Public().(ed25519.PublicKey), frame, sig), nil
}

func (g sessionGrant) write(w http.ResponseWriter, status int) {
	controller, target := sha256.Sum256(g.ControllerKey), sha256.Sum256(g.TargetKey)
	writeJSON(w, status, map[string]any{
		"session_id": g.ID, "grant_id": g.Grant, "organization_id": g.Organization, "user_id": g.User, "kid": g.Kid,
		"controller_device_id": g.Controller, "target_device_id": g.Target, "controller_rustdesk_id": g.ControllerRustDeskID,
		"target_rustdesk_id": g.TargetRustDeskID, "controller_key_sha256": hex.EncodeToString(controller[:]), "target_key_sha256": hex.EncodeToString(target[:]),
		"policy_version": g.PolicyVersion, "allowed_actions": controlActionsFromMask(byte(g.Mask)), "confirmation_mode": g.Mode,
		"purpose": g.Purpose, "issued_ms": g.Issued.UnixMilli(), "expires_ms": g.Expires.UnixMilli(),
		"signature": base64.RawURLEncoding.EncodeToString(g.Signature),
	})
}

// Existing ACL writes, policy changes and lifecycle mutations take this same
// organization lock before changing authorization inputs.
func lockSessionAuthorization(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT singleton FROM organizations WHERE singleton FOR NO KEY UPDATE")
	return err
}

func sessionPolicyAccess(ctx context.Context, tx pgx.Tx, org string, in connectionRequest, mode, rule string, revision int64) (connectionDecision, int64, byte, error) {
	decision, err := evaluateConnectionRuleVersion(ctx, tx, org, in, rule, revision)
	if err != nil || !decision.Allowed {
		return decision, 0, 0, err
	}
	policy, applied, err := effectiveManagedPolicy(ctx, tx, org, decision.Target, in.Purpose)
	if err != nil {
		return decision, 0, 0, err
	}
	mask := controlActionMask(in.Actions) & controlActionMask(policy.Actions)
	switch {
	case !applied:
		decision.Reason = "policy_not_applied"
	case mode == "unattended" && !policy.Unattended:
		decision.Reason = "unattended_not_allowed"
	case mask == 0:
		decision.Reason = "policy_actions_denied"
	default:
		return decision, policy.Version, mask, nil
	}
	decision.Allowed = false
	return decision, 0, 0, nil
}

type sessionEndpoints struct {
	ControllerKey, TargetKey                      []byte
	ControllerRustDeskID, TargetRustDeskID, Group string
}

func readSessionEndpoints(ctx context.Context, tx pgx.Tx, org, controller, target string) (sessionEndpoints, error) {
	var e sessionEndpoints
	err := tx.QueryRow(ctx, `SELECT c.public_key,d.public_key,c.rustdesk_id,d.rustdesk_id,d.group_id
		FROM devices c JOIN devices d ON d.organization_id=c.organization_id
		WHERE c.organization_id=$1 AND c.id=$2 AND d.id=$3`, org, controller, target).
		Scan(&e.ControllerKey, &e.TargetKey, &e.ControllerRustDeskID, &e.TargetRustDeskID, &e.Group)
	return e, err
}

func (g sessionGrant) revalidate(ctx context.Context, tx pgx.Tx) (string, string, error) {
	current, err := managedControllerSession(ctx, tx, g.ControllerDigest, true)
	if errors.Is(err, errManagedIdentity) || (err == nil && (current.User != g.User || current.Device != g.Controller ||
		current.Organization != g.Organization || current.Protocol != managedProtocolV1)) {
		return "controller_inactive", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return g.revalidateAccess(ctx, tx)
}

func (g sessionGrant) revalidateAccess(ctx context.Context, tx pgx.Tx) (string, string, error) {
	in := connectionRequest{User: g.User, Controller: g.Controller, Target: g.Target, Book: g.Book, Entry: g.Entry, Purpose: g.Purpose, Actions: g.Actions}
	if g.Book != "" {
		in.Target = ""
	}
	decision, version, mask, err := sessionPolicyAccess(ctx, tx, g.Organization, in, g.Mode, g.Rule, g.RuleVersion)
	if err != nil {
		return "", "", err
	}
	if !decision.Allowed {
		return decision.Reason, "", nil
	}
	if decision.Target != g.Target || version != g.PolicyVersion || mask&byte(g.Mask) != byte(g.Mask) {
		return "authorization_changed", "", nil
	}
	e, err := readSessionEndpoints(ctx, tx, g.Organization, g.Controller, g.Target)
	if err != nil {
		return "", "", err
	}
	if !bytes.Equal(e.ControllerKey, g.ControllerKey) || !bytes.Equal(e.TargetKey, g.TargetKey) ||
		e.ControllerRustDeskID != g.ControllerRustDeskID || e.TargetRustDeskID != g.TargetRustDeskID {
		return "endpoint_changed", "", nil
	}
	return "", e.Group, nil
}

func auditSession(ctx context.Context, tx pgx.Tx, g sessionGrant, group, action, result, reason string) error {
	_, err := addAuditEvent(ctx, tx, auditEvent{
		OrganizationID: g.Organization, ActorID: g.User, ResourceID: g.Target, DeviceGroupID: group,
		SessionID: g.ID, Action: action, Result: result, Reason: reason,
	})
	return err
}

func sessionClock(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, "SELECT date_trunc('milliseconds',clock_timestamp())").Scan(&now)
	return now, err
}

func (a *API) issueSessionGrant(w http.ResponseWriter, r *http.Request) {
	hash := managedBearer(r, managedUserPrefix)
	if hash == nil {
		failure(w, "issue session grant", errManagedIdentity)
		return
	}
	if a.managedKey == nil {
		failure(w, "issue session grant", errManagedUnavailable)
		return
	}
	var req sessionIssueRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Mode == "" {
		req.Mode = "attended"
	}
	if !userIDPattern.MatchString(req.RequestID) || (req.Mode != "attended" && req.Mode != "unattended") {
		failure(w, "issue session grant", errSessionInput)
		return
	}
	slices.Sort(req.Actions)
	payload, err := json.Marshal(req)
	if err != nil {
		failure(w, "encode session request", err)
		return
	}
	reqHash := sha256.Sum256(payload)
	ctx := r.Context()
	var output sessionGrant
	var rejected error
	status := http.StatusCreated
	err = pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		if err := lockSessionAuthorization(ctx, tx); err != nil {
			return err
		}
		who, err := managedControllerSession(ctx, tx, hash, true)
		if err != nil {
			return err
		}
		prior, err := readSessionGrant(tx.QueryRow(ctx, sessionGrantSelect+
			"WHERE organization_id=$1 AND user_id=$2 AND controller_device_id=$3 AND request_id=$4 FOR NO KEY UPDATE",
			who.Organization, who.User, who.Device, req.RequestID))
		if err == nil {
			if !bytes.Equal(prior.RequestHash, reqHash[:]) || !bytes.Equal(prior.ControllerDigest, hash) {
				rejected = errSessionConflict
				return auditSession(ctx, tx, prior, "", "session.grant_denied", "denied", "idempotency_conflict")
			}
			reason, group, err := prior.revalidate(ctx, tx)
			if err == nil {
				reason, err = sessionTerminalReason(ctx, tx, prior.ID, reason)
			}
			if err != nil {
				return err
			}
			now, err := sessionClock(ctx, tx)
			if err != nil {
				return err
			}
			if !now.Before(prior.Expires) || prior.Kid != a.managedKey.id {
				reason = "grant_expired"
			}
			if reason != "" {
				rejected = errSessionDenied
				return auditSession(ctx, tx, prior, group, "session.grant_denied", "denied", reason)
			}
			ok, err := prior.matchesSignature(a.managedKey, prior.Grant, prior.Signature)
			if err != nil {
				return err
			}
			if !ok {
				rejected = errSessionDenied
				return auditSession(ctx, tx, prior, group, "session.grant_denied", "denied", "signing_key_changed")
			}
			output, status = prior, http.StatusOK
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		in := connectionRequest{User: who.User, Controller: who.Device, Target: req.Target, RustDeskID: req.RustDeskID,
			Book: req.Book, Entry: req.Entry, Purpose: req.Purpose, Actions: req.Actions}
		if !in.valid() {
			return errSessionInput
		}
		decision, policy, mask, err := sessionPolicyAccess(ctx, tx, who.Organization, in, req.Mode, "", 0)
		if err != nil {
			return err
		}
		if !decision.Allowed {
			rejected = errSessionDenied
			return auditSession(ctx, tx, sessionGrant{Organization: who.Organization, User: who.User}, "", "session.grant_denied", "denied", decision.Reason)
		}
		e, err := readSessionEndpoints(ctx, tx, who.Organization, who.Device, decision.Target)
		if err != nil {
			return err
		}
		g := sessionGrant{Organization: who.Organization, User: who.User, Controller: who.Device, Target: decision.Target,
			ControllerRustDeskID: e.ControllerRustDeskID, TargetRustDeskID: e.TargetRustDeskID, ControllerKey: e.ControllerKey, TargetKey: e.TargetKey,
			Book: req.Book, Entry: req.Entry, Purpose: req.Purpose, Actions: req.Actions, Mode: req.Mode, ControllerDigest: hash,
			BrowserDigest: who.BrowserDigest, RequestHash: reqHash[:], Rule: decision.Rule, RuleVersion: decision.RuleVersion, PolicyVersion: policy, Mask: int16(mask), Kid: a.managedKey.id}
		if err := tx.QueryRow(ctx, "SELECT gen_random_uuid(),gen_random_uuid(),date_trunc('milliseconds',clock_timestamp())").
			Scan(&g.ID, &g.Grant, &g.Issued); err != nil {
			return err
		}
		g.Expires = g.Issued.Add(time.Minute)
		frame, err := g.signingFrame()
		if err != nil {
			return err
		}
		g.Signature = ed25519.Sign(a.managedKey.key, frame)
		_, err = tx.Exec(ctx, `INSERT INTO managed_sessions(id,grant_id,organization_id,user_id,controller_device_id,target_device_id,
			controller_rustdesk_id,target_rustdesk_id,controller_digest,request_id,request_hash,book_id,entry_id,purpose,
			requested_actions,controller_key,target_key,policy_version,rule_id,rule_version,allowed_mask,confirmation_mode,signing_kid,signature,issued_at,expires_at,browser_digest)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,'')::uuid,NULLIF($13,'')::uuid,
				$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
			g.ID, g.Grant, g.Organization, g.User, g.Controller, g.Target, g.ControllerRustDeskID, g.TargetRustDeskID,
			g.ControllerDigest, req.RequestID, g.RequestHash, g.Book, g.Entry, g.Purpose, g.Actions, g.ControllerKey, g.TargetKey,
			g.PolicyVersion, g.Rule, g.RuleVersion, g.Mask, g.Mode, g.Kid, g.Signature, g.Issued, g.Expires, g.BrowserDigest)
		if err != nil {
			return err
		}
		output = g
		return auditSession(ctx, tx, g, e.Group, "session.grant_issued", "allowed", "")
	})
	if err == nil {
		err = rejected
	}
	if err != nil {
		failure(w, "issue session grant", err)
		return
	}
	output.write(w, status)
}
