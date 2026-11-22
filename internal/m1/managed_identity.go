package m1

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"deskrealm/internal/managedproto"

	"github.com/jackc/pgx/v5"
)

const (
	managedDevicePrefix = "drd_"
	managedUserPrefix   = "dru_"
	managedProtocolV1   = 1
)

var (
	errManagedIdentity    = errors.New("managed_identity_denied")
	errManagedUnavailable = errors.New("managed_issuance_unavailable")
	reportedBuild         = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,63}$`)
)

type runtimeDevice struct {
	ID, Organization, Group string
	Epoch                   int64
	Key                     ed25519.PublicKey
}

type identityChallenge struct {
	ID           string `json:"challenge_id"`
	Organization string `json:"organization_id"`
	Device       string `json:"device_id"`
	User         string `json:"user_id,omitempty"`
	Nonce        string `json:"nonce"`
	Flow         string `json:"flow,omitempty"`
	KeyEpoch     int64  `json:"key_epoch"`
	ExpiresMS    int64  `json:"expires_ms"`
	Protocol     uint16 `json:"protocol_version"`
}

type managedCredential struct {
	Value     string    `json:"credential"`
	ExpiresAt time.Time `json:"expires_at"`
}

func lockRuntimeDevice(ctx context.Context, tx pgx.Tx, id string) (runtimeDevice, error) {
	result := runtimeDevice{ID: id}
	err := tx.QueryRow(ctx, `SELECT organization_id,group_id,device_key_epoch,public_key FROM devices
		WHERE id=$1 AND registration_state='approved' FOR NO KEY UPDATE`, id).
		Scan(&result.Organization, &result.Group, &result.Epoch, &result.Key)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errManagedIdentity
	}
	return result, err
}

func managedDigest(value, prefix string) []byte {
	raw, found := strings.CutPrefix(value, prefix)
	if !found || !validToken(raw) {
		return nil
	}
	return digest(value)
}

func managedBearer(r *http.Request, prefix string) []byte {
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 {
		return nil
	}
	scheme, value, _ := strings.Cut(auth[0], " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return nil
	}
	return managedDigest(value, prefix)
}

func (d runtimeDevice) credential(ctx context.Context, tx pgx.Tx, hash []byte) (uint16, time.Time, error) {
	var protocol uint16
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT protocol_version,expires_at FROM managed_device_credentials
		WHERE digest=$1 AND device_id=$2 AND key_epoch=$3 AND expires_at>clock_timestamp()`,
		hash, d.ID, d.Epoch).Scan(&protocol, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errManagedIdentity
	}
	return protocol, expires, err
}

func challengeProof(c identityChallenge) ([]byte, error) {
	uuid := func(value string) []byte {
		out, _ := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
		return out
	}
	nonce, _ := base64.RawURLEncoding.Strict().DecodeString(c.Nonce)
	epoch := binary.BigEndian.AppendUint64(nil, uint64(c.KeyEpoch))
	expiry := binary.BigEndian.AppendUint64(nil, uint64(c.ExpiresMS))
	if c.User != "" {
		flow, _ := base64.RawURLEncoding.Strict().DecodeString(c.Flow)
		return managedproto.Encode(managedproto.UserBinding, uuid(c.Organization), uuid(c.Device), uuid(c.User),
			uuid(c.ID), nonce, digest(string(flow)), epoch, expiry)
	}
	return managedproto.Encode(managedproto.DeviceChallenge, uuid(c.Organization), uuid(c.Device), uuid(c.ID),
		nonce, epoch, expiry, binary.BigEndian.AppendUint16(nil, c.Protocol))
}

// Identity requests lock the device first. Browser binding already holds the
// organization and authenticated browser session locks before reaching here.
func issueIdentityChallenge(ctx context.Context, tx pgx.Tx, d runtimeDevice, protocol uint16, who string,
	deviceHash, browserHash []byte) (identityChallenge, error) {
	c := identityChallenge{Organization: d.Organization, Device: d.ID, User: who, KeyEpoch: d.Epoch, Protocol: protocol}
	nonce, err := token()
	if err != nil {
		return c, err
	}
	var flow *string
	if browserHash != nil {
		value, err := token()
		if err != nil {
			return c, err
		}
		flow = &value
	}
	err = tx.QueryRow(ctx, `INSERT INTO managed_identity_challenges AS current
		(device_id,key_epoch,protocol_version,nonce,device_digest,browser_digest,flow,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,date_trunc('milliseconds',clock_timestamp())+interval '60 seconds')
		ON CONFLICT(device_id,device_digest,browser_digest) DO UPDATE SET
		id=EXCLUDED.id,key_epoch=EXCLUDED.key_epoch,protocol_version=EXCLUDED.protocol_version,nonce=EXCLUDED.nonce,
		flow=EXCLUDED.flow,expires_at=EXCLUDED.expires_at,consumed_at=NULL
		WHERE current.consumed_at IS NOT NULL OR current.expires_at<=clock_timestamp() OR current.key_epoch<>EXCLUDED.key_epoch
		RETURNING id,nonce,COALESCE(flow,''),(extract(epoch FROM expires_at)*1000)::bigint`,
		d.ID, d.Epoch, protocol, nonce, deviceHash, browserHash, flow).Scan(&c.ID, &c.Nonce, &c.Flow, &c.ExpiresMS)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id,nonce,COALESCE(flow,''),(extract(epoch FROM expires_at)*1000)::bigint
			FROM managed_identity_challenges WHERE device_id=$1 AND device_digest IS NOT DISTINCT FROM $2::bytea
			AND browser_digest IS NOT DISTINCT FROM $3::bytea`, d.ID, deviceHash, browserHash).
			Scan(&c.ID, &c.Nonce, &c.Flow, &c.ExpiresMS)
	}
	return c, err
}

func consumeIdentityChallenge(ctx context.Context, tx pgx.Tx, d runtimeDevice, id, who string, browserHash, signature []byte) (identityChallenge, []byte, error) {
	c := identityChallenge{ID: id, Organization: d.Organization, Device: d.ID, User: who, KeyEpoch: d.Epoch}
	var deviceHash []byte
	err := tx.QueryRow(ctx, `SELECT nonce,COALESCE(flow,''),protocol_version,(extract(epoch FROM expires_at)*1000)::bigint,device_digest
		FROM managed_identity_challenges WHERE id=$1 AND device_id=$2 AND key_epoch=$3
		AND browser_digest IS NOT DISTINCT FROM $4::bytea AND consumed_at IS NULL AND expires_at>clock_timestamp()`,
		id, d.ID, d.Epoch, browserHash).Scan(&c.Nonce, &c.Flow, &c.Protocol, &c.ExpiresMS, &deviceHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil, errManagedIdentity
	}
	if err != nil {
		return c, nil, err
	}
	message, err := challengeProof(c)
	if err != nil {
		return c, nil, err
	}
	if !ed25519.Verify(d.Key, message, signature) {
		return c, nil, errManagedIdentity
	}
	tag, err := tx.Exec(ctx, `UPDATE managed_identity_challenges SET consumed_at=clock_timestamp()
		WHERE id=$1 AND consumed_at IS NULL AND expires_at>clock_timestamp()`, id)
	if err == nil && tag.RowsAffected() != 1 {
		err = errManagedIdentity
	}
	return c, deviceHash, err
}

func issueRuntimeCredential(ctx context.Context, tx pgx.Tx, d runtimeDevice, c identityChallenge, deviceHash, browserHash []byte) (managedCredential, error) {
	var result managedCredential
	value, err := token()
	if err != nil {
		return result, err
	}
	if browserHash == nil {
		result.Value = managedDevicePrefix + value
		if _, err := tx.Exec(ctx, `DELETE FROM managed_device_credentials
			WHERE device_id=$1 AND (key_epoch<>$2 OR expires_at<=clock_timestamp())`, d.ID, d.Epoch); err != nil {
			return result, err
		}
		err = tx.QueryRow(ctx, `INSERT INTO managed_device_credentials(digest,device_id,key_epoch,protocol_version,expires_at)
			VALUES($1,$2,$3,$4,clock_timestamp()+interval '15 minutes') RETURNING expires_at`,
			digest(result.Value), d.ID, d.Epoch, c.Protocol).Scan(&result.ExpiresAt)
	} else {
		var deviceExpiry time.Time
		_, deviceExpiry, err = d.credential(ctx, tx, deviceHash)
		if err != nil {
			return result, err
		}
		result.Value = managedUserPrefix + value
		if _, err := tx.Exec(ctx, `DELETE FROM managed_controller_credentials WHERE device_digest=$1 AND browser_digest=$2
			AND expires_at<=clock_timestamp()`, deviceHash, browserHash); err != nil {
			return result, err
		}
		err = tx.QueryRow(ctx, `INSERT INTO managed_controller_credentials(digest,device_digest,browser_digest,expires_at)
			SELECT $1,$2,s.token_digest,least(s.expires_at,$4,clock_timestamp()+interval '15 minutes')
			FROM browser_sessions s WHERE s.token_digest=$3 AND s.revoked_at IS NULL
			AND s.expires_at>clock_timestamp() AND $4::timestamptz>clock_timestamp() RETURNING expires_at`,
			digest(result.Value), deviceHash, browserHash, deviceExpiry).Scan(&result.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			err = errManagedIdentity
		}
	}
	if err != nil {
		return result, err
	}
	action := "device.runtime_authenticated"
	if c.User != "" {
		action = "device.controller_bound"
	}
	_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: d.Organization, ActorID: c.User,
		ResourceID: d.ID, DeviceGroupID: d.Group, Action: action, Result: "success"})
	return result, err
}

func (a *API) managedSigningKey(w http.ResponseWriter, _ *http.Request) {
	if a.managedKey == nil {
		failure(w, "read managed signing key", errManagedUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ID     string `json:"kid"`
		Public string `json:"public_key"`
	}{a.managedKey.id, base64.RawURLEncoding.EncodeToString(a.managedKey.key.Public().(ed25519.PublicKey))})
}

func (a *API) managedChallenge(w http.ResponseWriter, r *http.Request) {
	binding := r.URL.Path == "/v1/managed/users/challenge"
	if a.managedKey == nil {
		failure(w, "issue managed challenge", errManagedUnavailable)
		return
	}
	if binding {
		if _, ok := a.sessionWrite(w, r); !ok {
			return
		}
	}
	var in struct {
		Device     string `json:"device_id"`
		Protocol   uint16 `json:"protocol_version"`
		Credential string `json:"device_credential"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	deviceHash := managedDigest(in.Credential, managedDevicePrefix)
	if !userIDPattern.MatchString(in.Device) || (binding && (deviceHash == nil || in.Protocol != 0)) ||
		(!binding && (in.Credential != "" || in.Protocol != managedProtocolV1)) {
		failure(w, "issue managed challenge", errManagedIdentity)
		return
	}
	ctx := r.Context()
	var result identityChallenge
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		who, browserHash, err := a.bindingActor(r, tx, binding)
		if err != nil {
			return err
		}
		d, err := lockRuntimeDevice(ctx, tx, in.Device)
		if err != nil {
			return err
		}
		if binding {
			if who.Organization != d.Organization {
				return errManagedIdentity
			}
			in.Protocol, _, err = d.credential(ctx, tx, deviceHash)
			if err != nil {
				return err
			}
		}
		result, err = issueIdentityChallenge(ctx, tx, d, in.Protocol, who.ID, deviceHash, browserHash)
		return err
	})
	if err != nil {
		failure(w, "issue managed challenge", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) bindingActor(r *http.Request, tx pgx.Tx, required bool) (actor, []byte, error) {
	if !required {
		return actor{}, nil, nil
	}
	value := a.cookieValue(r)
	who, err := actorSession(r.Context(), tx, value, true)
	return who, digest(value), err
}

func (a *API) managedComplete(w http.ResponseWriter, r *http.Request) {
	binding := r.URL.Path == "/v1/managed/users/complete"
	if a.managedKey == nil {
		failure(w, "complete managed challenge", errManagedUnavailable)
		return
	}
	if binding {
		if _, ok := a.sessionWrite(w, r); !ok {
			return
		}
	}
	var in struct {
		Device    string `json:"device_id"`
		Challenge string `json:"challenge_id"`
		Signature string `json:"signature"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(in.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize ||
		base64.RawURLEncoding.EncodeToString(signature) != in.Signature ||
		!userIDPattern.MatchString(in.Device) || !userIDPattern.MatchString(in.Challenge) {
		failure(w, "complete managed challenge", errManagedIdentity)
		return
	}
	ctx := r.Context()
	var result managedCredential
	err = pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		who, browserHash, err := a.bindingActor(r, tx, binding)
		if err != nil {
			return err
		}
		d, err := lockRuntimeDevice(ctx, tx, in.Device)
		if err != nil {
			return err
		}
		if binding && who.Organization != d.Organization {
			return errManagedIdentity
		}
		c, deviceHash, err := consumeIdentityChallenge(ctx, tx, d, in.Challenge, who.ID, browserHash, signature)
		if err != nil {
			return err
		}
		result, err = issueRuntimeCredential(ctx, tx, d, c, deviceHash, browserHash)
		return err
	})
	if err != nil {
		failure(w, "complete managed challenge", err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

type managedController struct {
	User         string `json:"user_id"`
	Device       string `json:"device_id"`
	Organization string `json:"organization_id"`
	Protocol     uint16 `json:"protocol_version"`
}

// Callers that create grants must revalidate this identity inside the grant
// transaction under the same locks as the affected user and device.
func (s Store) managedControllerSession(ctx context.Context, hash []byte) (managedController, error) {
	var result managedController
	if hash == nil {
		return result, errManagedIdentity
	}
	err := s.DB.QueryRow(ctx, `SELECT u.id,d.id,d.organization_id,k.protocol_version
		FROM managed_controller_credentials c JOIN managed_device_credentials k ON k.digest=c.device_digest
		JOIN devices d ON d.id=k.device_id JOIN browser_sessions s ON s.token_digest=c.browser_digest
		JOIN users u ON u.id=s.user_id AND u.organization_id=d.organization_id
		WHERE c.digest=$1 AND c.expires_at>clock_timestamp() AND k.expires_at>clock_timestamp()
		AND s.expires_at>clock_timestamp() AND s.revoked_at IS NULL AND u.status='active'
		AND d.registration_state='approved' AND d.device_key_epoch=k.key_epoch`, hash).
		Scan(&result.User, &result.Device, &result.Organization, &result.Protocol)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errManagedIdentity
	}
	return result, err
}

func (a *API) managedUserSession(w http.ResponseWriter, r *http.Request) {
	who, err := a.store.managedControllerSession(r.Context(), managedBearer(r, managedUserPrefix))
	if err != nil {
		failure(w, "read managed user session", err)
		return
	}
	writeJSON(w, http.StatusOK, who)
}

func (a *API) managedHeartbeat(w http.ResponseWriter, r *http.Request) {
	hash := managedBearer(r, managedDevicePrefix)
	if hash == nil {
		failure(w, "managed heartbeat", errManagedIdentity)
		return
	}
	var in struct {
		Device string `json:"device_id"`
		Build  string `json:"reported_build"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(in.Device) || !reportedBuild.MatchString(in.Build) {
		writeError(w, http.StatusBadRequest, "invalid_heartbeat")
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		d, err := lockRuntimeDevice(ctx, tx, in.Device)
		if err != nil {
			return err
		}
		_, expires, err := d.credential(ctx, tx, hash)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE devices SET managed_heartbeat_at=clock_timestamp(),
			managed_heartbeat_epoch=$2,managed_reported_build=$3 WHERE id=$1 AND $4::timestamptz>clock_timestamp()`,
			d.ID, d.Epoch, in.Build, expires)
		if err == nil && tag.RowsAffected() != 1 {
			return errManagedIdentity
		}
		return err
	})
	if err != nil {
		failure(w, "managed heartbeat", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
