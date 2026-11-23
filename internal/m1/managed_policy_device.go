package m1

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"

	"deskrealm/internal/managedproto"

	"github.com/jackc/pgx/v5"
)

var policyReceiptFields = [...]string{"allowed_actions", "unattended_purposes", "schema_version"}

type policyReceipt struct {
	Device   string   `json:"device_id"`
	Delivery string   `json:"delivery_id"`
	Version  int64    `json:"policy_version"`
	SHA256   string   `json:"payload_sha256"`
	Result   string   `json:"result"`
	Failed   []string `json:"failed_fields"`
	Signed   string   `json:"signature"`
}

func (r policyReceipt) frameFields() (result, mask byte, ok bool) {
	for _, wanted := range r.Failed {
		var bit byte
		for i, field := range policyReceiptFields {
			if wanted == field {
				bit = 1 << i
				break
			}
		}
		if bit == 0 || mask&bit != 0 {
			return 0, 0, false
		}
		mask |= bit
	}
	if r.Result == "applied" && mask == 0 {
		return 1, 0, true
	}
	if r.Result == "failed" && mask != 0 {
		return 2, mask, true
	}
	return 0, 0, false
}

func (a *API) getRuntimePolicy(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	hash := managedBearer(r, managedDevicePrefix)
	if hash == nil || !userIDPattern.MatchString(deviceID) {
		failure(w, "fetch device policy", errManagedIdentity)
		return
	}
	ctx := r.Context()
	var policy *policyDelivery
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		d, err := lockRuntimeDevice(ctx, tx, deviceID)
		if err != nil {
			return err
		}
		if _, _, err := d.credential(ctx, tx, hash); err != nil {
			return err
		}
		policy, err = currentPolicyDelivery(ctx, tx, d)
		return err
	})
	if err != nil {
		failure(w, "fetch device policy", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Policy *policyDelivery `json:"policy"`
	}{policy})
}

func (a *API) receiveRuntimePolicy(w http.ResponseWriter, r *http.Request) {
	hash := managedBearer(r, managedDevicePrefix)
	if hash == nil {
		failure(w, "receive device policy", errManagedIdentity)
		return
	}
	var in policyReceipt
	if !readJSON(w, r, &in) {
		return
	}
	result, mask, ok := in.frameFields()
	sum, hashErr := hex.DecodeString(in.SHA256)
	signature, signatureErr := base64.RawURLEncoding.Strict().DecodeString(in.Signed)
	if !userIDPattern.MatchString(in.Device) || !userIDPattern.MatchString(in.Delivery) || in.Version < 1 ||
		!ok || hashErr != nil || len(sum) != 32 || hex.EncodeToString(sum) != in.SHA256 ||
		signatureErr != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != in.Signed {
		failure(w, "receive device policy", errPolicyInput)
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
		current, err := currentPolicyDelivery(ctx, tx, d)
		if err != nil {
			return err
		}
		if current == nil || current.ID != in.Delivery || current.Version != in.Version || current.SHA256 != in.SHA256 {
			return errPolicyStale
		}
		message, err := managedproto.Encode(managedproto.PolicyReceipt, managedFrameUUID(d.Organization), managedFrameUUID(d.ID),
			managedFrameUUID(current.ID), binary.BigEndian.AppendUint64(nil, uint64(in.Version)), sum,
			binary.BigEndian.AppendUint64(nil, uint64(d.Epoch)), []byte{result}, []byte{mask})
		if err != nil {
			return err
		}
		if !ed25519.Verify(d.Key, message, signature) {
			return errManagedIdentity
		}
		var storedResult string
		var storedMask int16
		err = tx.QueryRow(ctx, `SELECT result,failed_mask FROM managed_policy_receipts WHERE delivery_id=$1`, in.Delivery).
			Scan(&storedResult, &storedMask)
		switch {
		case err == nil && storedResult == in.Result && storedMask == int16(mask):
			return nil
		case err == nil:
			return errPolicyStale
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO managed_policy_receipts(delivery_id,result,failed_mask,signature,signer_key)
			SELECT $1,$2,$3,$4,$5 WHERE $6::timestamptz>clock_timestamp()`,
			in.Delivery, in.Result, int16(mask), signature, d.Key, expires)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errManagedIdentity
		}
		if result == 1 {
			if _, err := tx.Exec(ctx, "UPDATE devices SET applied_policy_version=$2 WHERE id=$1", d.ID, in.Version); err != nil {
				return err
			}
		}
		// The resource is the device. The reason is the immutable delivery ID.
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: d.Organization, ResourceID: d.ID, DeviceGroupID: d.Group,
			Action: "device.policy_reported", Result: in.Result, Reason: in.Delivery})
		return err
	})
	if err != nil {
		failure(w, "receive device policy", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readDevicePolicyStatus(ctx context.Context, tx pgx.Tx, who actor, id string) (devicePolicyStatus, error) {
	var out devicePolicyStatus
	var mask int16
	err := tx.QueryRow(ctx, `SELECT COALESCE(d.managed_policy_owner_id::text,''),d.version,
		COALESCE(p.id::text,''),d.expected_policy_version,d.applied_policy_version,
		CASE WHEN p.id IS NULL THEN 'unassigned' WHEN r.result IS NULL THEN 'pending' ELSE r.result END,
		COALESCE(r.failed_mask,0),r.received_at FROM devices d JOIN device_groups dg ON d.group_id=dg.id
		LEFT JOIN managed_policy_deliveries p ON p.id=d.managed_policy_delivery_id
		LEFT JOIN managed_policy_receipts r ON r.delivery_id=p.id
		WHERE d.organization_id=$1 AND `+deviceGroupRead+` AND d.id=$4`, who.Organization, who.ID, who.PlatformAdmin, id).
		Scan(&out.Owner, &out.DeviceVersion, &out.DeliveryID, &out.ExpectedVersion, &out.AppliedVersion, &out.State, &mask, &out.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errDeviceMissing
	}
	for i, field := range policyReceiptFields {
		if mask&(1<<i) != 0 {
			out.FailedFields = append(out.FailedFields, field)
		}
	}
	return out, err
}

func (a *API) devicePolicyStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "read device policy status", errDeviceMissing)
		return
	}
	var out devicePolicyStatus
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		var err error
		out, err = readDevicePolicyStatus(r.Context(), tx, who, id)
		return err
	})
	if err != nil {
		failure(w, "read device policy status", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) changePolicyOwner(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	var in struct {
		Owner   *string `json:"owner_user_id"`
		Version int64   `json:"device_version"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(id) || in.Version < 1 || in.Owner == nil ||
		(*in.Owner != "" && !userIDPattern.MatchString(*in.Owner)) {
		failure(w, "change device policy owner", errPolicyInput)
		return
	}
	ctx := r.Context()
	var out devicePolicyStatus
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		d, err := lockManagedDevice(ctx, tx, who, id)
		if err != nil {
			return err
		}
		if d.Version != in.Version {
			return errDeviceVersion
		}
		if d.RegistrationState == "retired" {
			return errDeviceLifecycle
		}
		tag, err := tx.Exec(ctx, `UPDATE devices SET managed_policy_owner_id=NULLIF($2,'')::uuid,version=version+1
			WHERE id=$1 AND ($2='' OR EXISTS(
				SELECT 1 FROM users WHERE organization_id=$3 AND id=NULLIF($2,'')::uuid AND status='active'
			))`, id, *in.Owner, who.Organization)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errPolicyInput
		}
		if err := refreshManagedPolicyTargets(ctx, tx, who.Organization, id, false); err != nil {
			return err
		}
		out, err = readDevicePolicyStatus(ctx, tx, who, id)
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "device.policy_owner_changed", id, strconv.FormatInt(d.Version+1, 10))
	})
	if err != nil {
		failure(w, "change device policy owner", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) retryDevicePolicy(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	var in struct {
		Delivery string `json:"delivery_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(id) || !userIDPattern.MatchString(in.Delivery) {
		failure(w, "retry device policy", errPolicyInput)
		return
	}
	ctx := r.Context()
	var out devicePolicyStatus
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		d, err := lockManagedDevice(ctx, tx, who, id)
		if err != nil {
			return err
		}
		out, err = readDevicePolicyStatus(ctx, tx, who, id)
		if err != nil {
			return err
		}
		if d.RegistrationState != "approved" || out.State != "failed" || out.DeliveryID != in.Delivery {
			return errPolicyStale
		}
		if err := refreshManagedPolicyTargets(ctx, tx, who.Organization, id, true); err != nil {
			return err
		}
		out, err = readDevicePolicyStatus(ctx, tx, who, id)
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "device.policy_retried", id, in.Delivery)
	})
	if err != nil {
		failure(w, "retry device policy", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
