package m1

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const auditAccess = `($3 OR EXISTS (
	SELECT 1 FROM audit_grants g WHERE g.organization_id=e.organization_id AND g.user_id=$2
	AND (NOT $4 OR g.can_export)
	AND (g.scope='organization' OR (g.scope='self' AND e.actor_id=$2)
		OR (g.scope='device_group' AND g.device_group_id=e.device_group_id)
		OR (g.scope='user_group' AND g.user_group_id=ANY(e.user_group_ids)))
))`

const auditProjection = `e.id, COALESCE(e.actor_id::text,''), COALESCE(u.username,''), e.action,e.result,e.source,
	COALESCE(e.resource_id::text,''), COALESCE(e.session_id::text,''), COALESCE(e.task_id::text,''),
	COALESCE(e.device_group_id::text,''), e.occurred_at,e.received_at,e.receipt_state,
	COALESCE(e.receipt_result,''),e.acknowledged_at,e.duplicate_receipts`

type auditRow struct {
	ID                string     `json:"id"`
	ActorID           string     `json:"actor_id,omitempty"`
	ActorName         string     `json:"actor_name,omitempty"`
	Action            string     `json:"action"`
	Result            string     `json:"result"`
	Source            string     `json:"source"`
	ResourceID        string     `json:"resource_id,omitempty"`
	SessionID         string     `json:"session_id,omitempty"`
	TaskID            string     `json:"task_id,omitempty"`
	DeviceGroupID     string     `json:"device_group_id,omitempty"`
	OccurredAt        time.Time  `json:"occurred_at"`
	ReceivedAt        time.Time  `json:"received_at"`
	ReceiptState      string     `json:"receipt_state"`
	ReceiptResult     string     `json:"receipt_result,omitempty"`
	AcknowledgedAt    *time.Time `json:"acknowledged_at,omitempty"`
	DuplicateReceipts int        `json:"duplicate_receipts"`
}

type auditPage struct {
	Events    []auditRow `json:"events"`
	Next      string     `json:"next,omitempty"`
	CanExport bool       `json:"can_export"`
}

type auditDetail struct {
	auditRow
	Reason string `json:"reason,omitempty"`
}

type auditFilter struct {
	action, status, session, task, resource string
	from, to, before                        *time.Time
	beforeID                                string
}

func parseAuditFilter(query string) (auditFilter, error) {
	var f auditFilter
	values, err := url.ParseQuery(query)
	if err != nil {
		return f, errAuditInvalid
	}
	for field, entries := range values {
		if len(entries) != 1 || len(entries[0]) > 180 || entries[0] == "" {
			return f, errAuditInvalid
		}
		raw := entries[0]
		switch field {
		case "action":
			if !auditCode.MatchString(raw) {
				return f, errAuditInvalid
			}
			f.action = raw
		case "status":
			if raw != "confirmed" && raw != "pending" && raw != "duplicate" {
				return f, errAuditInvalid
			}
			f.status = raw
		case "session_id", "task_id", "resource_id":
			if !userIDPattern.MatchString(raw) {
				return f, errAuditInvalid
			}
			switch field {
			case "session_id":
				f.session = raw
			case "task_id":
				f.task = raw
			case "resource_id":
				f.resource = raw
			}
		case "from", "to":
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil || parsed.Year() < 1 {
				return f, errAuditInvalid
			}
			if field == "from" {
				f.from = &parsed
			} else {
				f.to = &parsed
			}
		case "before":
			decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
			if err != nil {
				return f, errAuditInvalid
			}
			stamp, id, ok := strings.Cut(string(decoded), "|")
			parsed, err := time.Parse(time.RFC3339Nano, stamp)
			if !ok || err != nil || parsed.Year() < 1 || !userIDPattern.MatchString(id) {
				return f, errAuditInvalid
			}
			f.before, f.beforeID = &parsed, id
		default:
			return f, errAuditInvalid
		}
	}
	if f.from != nil && f.to != nil && !f.from.Before(*f.to) {
		return f, errAuditInvalid
	}
	return f, nil
}

func auditCapabilities(ctx context.Context, tx pgx.Tx, who actor) (bool, bool, error) {
	if who.PlatformAdmin {
		return true, true, nil
	}
	var read, export bool
	err := tx.QueryRow(ctx, `SELECT count(*)>0,COALESCE(bool_or(can_export),false)
		FROM audit_grants WHERE organization_id=$1 AND user_id=$2`, who.Organization, who.ID).Scan(&read, &export)
	return read, export, err
}

func auditQuery(ctx context.Context, tx pgx.Tx, who actor, f auditFilter, id string, export bool, limit int) ([]auditRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+auditProjection+` FROM audit_events e LEFT JOIN users u ON u.id=e.actor_id
		WHERE e.organization_id=$1 AND `+auditAccess+`
		AND ($5='' OR e.id=NULLIF($5,'')::uuid) AND ($6='' OR e.action=$6)
		AND ($7='' OR e.receipt_state=$7 OR ($7='duplicate' AND e.duplicate_receipts>0))
		AND ($8='' OR e.session_id=NULLIF($8,'')::uuid) AND ($9='' OR e.task_id=NULLIF($9,'')::uuid)
		AND ($10='' OR e.resource_id=NULLIF($10,'')::uuid)
		AND ($11::timestamptz IS NULL OR e.occurred_at >= $11) AND ($12::timestamptz IS NULL OR e.occurred_at < $12)
		AND ($13::timestamptz IS NULL OR (e.occurred_at,e.id)<($13,NULLIF($14,'')::uuid))
		ORDER BY e.occurred_at DESC,e.id DESC LIMIT $15`,
		who.Organization, who.ID, who.PlatformAdmin, export, id, f.action, f.status, f.session, f.task, f.resource, f.from, f.to, f.before, f.beforeID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]auditRow, 0)
	return pgx.AppendRows(out, rows, pgx.RowToStructByPos[auditRow])
}

func (a *API) withActor(r *http.Request, write bool, run func(pgx.Tx, actor) error) error {
	ctx := r.Context()
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if write {
		options = pgx.TxOptions{}
	}
	return pgx.BeginTxFunc(ctx, a.store.DB, options, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), write)
		if err != nil {
			return err
		}
		return run(tx, who)
	})
}

func (a *API) readAudit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id != "" && !userIDPattern.MatchString(id) {
		failure(w, "read audit event", errAuditMissing)
		return
	}
	f, err := parseAuditFilter(r.URL.RawQuery)
	if err != nil || (id != "" && r.URL.RawQuery != "") {
		failure(w, "read audit query", errAuditInvalid)
		return
	}
	ctx := r.Context()
	var out auditPage
	var detail auditDetail
	err = a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		read, export, err := auditCapabilities(ctx, tx, who)
		if err != nil {
			return err
		}
		if !read {
			if id != "" {
				return errAuditMissing
			}
			return errForbidden
		}
		out.CanExport = export
		out.Events, err = auditQuery(ctx, tx, who, f, id, false, 101)
		if err == nil && id != "" && len(out.Events) == 1 {
			detail.auditRow = out.Events[0]
			// Only server-generated metadata goes to the console. Operator-supplied
			// comments may contain secrets and are excluded here and from exports.
			switch detail.Action {
			case "address_book.shared", "address_book.share_revoked", "audit.granted", "audit.exported",
				"audit.retention_changed", "audit.retention_pruned":
				err = tx.QueryRow(ctx, `SELECT COALESCE(reason,'') FROM audit_events WHERE id=$1`, id).Scan(&detail.Reason)
			}
		}
		return err
	})
	if err != nil {
		failure(w, "read audit events", err)
		return
	}
	if id != "" {
		if len(out.Events) == 0 {
			failure(w, "read audit event", errAuditMissing)
			return
		}
		writeJSON(w, 200, detail)
		return
	}
	if len(out.Events) > 100 {
		out.Events = out.Events[:100]
		last := out.Events[99]
		out.Next = base64.RawURLEncoding.EncodeToString([]byte(last.OccurredAt.Format(time.RFC3339Nano) + "|" + last.ID))
	}
	writeJSON(w, 200, out)
}

var errAuditExportLimit = errors.New("audit_export_limit")

func (a *API) exportAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	f, err := parseAuditFilter(r.URL.RawQuery)
	if err != nil || f.before != nil {
		failure(w, "export audit query", errAuditInvalid)
		return
	}
	ctx := r.Context()
	var out []auditRow
	err = a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		_, export, err := auditCapabilities(ctx, tx, who)
		if err != nil {
			return err
		}
		if !export {
			return errForbidden
		}
		out, err = auditQuery(ctx, tx, who, f, "", true, 1001)
		if err != nil {
			return err
		}
		if len(out) > 1000 {
			return errAuditExportLimit
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "audit.exported", "", strconv.Itoa(len(out)))
	})
	if err != nil {
		failure(w, "export audit", err)
		return
	}

	var data bytes.Buffer
	csvWriter := csv.NewWriter(&data)
	_ = csvWriter.Write([]string{"event_id", "actor_id", "actor", "action", "result", "source", "resource_id", "session_id", "task_id",
		"device_group_id", "occurred_at", "received_at", "receipt_state", "receipt_result", "acknowledged_at", "duplicate_receipts"})
	for _, e := range out {
		acknowledged := ""
		if e.AcknowledgedAt != nil {
			acknowledged = e.AcknowledgedAt.UTC().Format(time.RFC3339Nano)
		}
		// Export contains only fixed-vocabulary fields, UUIDs, timestamps and
		// validated account names. Free-form notes and reasons stay in storage.
		_ = csvWriter.Write([]string{e.ID, e.ActorID, e.ActorName, e.Action, e.Result, e.Source, e.ResourceID, e.SessionID, e.TaskID,
			e.DeviceGroupID, e.OccurredAt.UTC().Format(time.RFC3339Nano), e.ReceivedAt.UTC().Format(time.RFC3339Nano),
			e.ReceiptState, e.ReceiptResult, acknowledged, strconv.Itoa(e.DuplicateReceipts)})
	}
	csvWriter.Flush()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="deskrealm-audit.csv"`)
	_, _ = w.Write(data.Bytes())
}
