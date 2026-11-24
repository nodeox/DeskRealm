package m1

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type overviewMetrics struct {
	ActiveSessions   int64  `json:"active_sessions"`
	Users            *int64 `json:"users,omitempty"`
	OnlineDevices    *int64 `json:"online_devices,omitempty"`
	PendingApprovals *int64 `json:"pending_approvals,omitempty"`
	AddressBooks     int64  `json:"address_books"`
	RetryableTargets *int64 `json:"retryable_targets,omitempty"`
	AvailableRelays  *int64 `json:"available_relays,omitempty"`
}

type overviewEvent struct {
	ID           string    `json:"id"`
	Action       string    `json:"action"`
	OccurredAt   time.Time `json:"occurred_at"`
	ReceiptState string    `json:"receipt_state"`
}

type overviewSnapshot struct {
	Organization           string          `json:"organization"`
	CanManage              bool            `json:"can_manage"`
	CanReadUsers           bool            `json:"can_read_users"`
	CanReadDevices         bool            `json:"can_read_devices"`
	CanReadAudit           bool            `json:"can_read_audit"`
	CanReadTasks           bool            `json:"can_read_tasks"`
	CanReadWindowsReleases bool            `json:"can_read_windows_releases,omitempty"`
	Metrics                overviewMetrics `json:"metrics"`
	RecentTasks            []taskSummary   `json:"recent_tasks"`
	RecentAudit            []overviewEvent `json:"recent_audit"`
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result := overviewSnapshot{RecentTasks: []taskSummary{}, RecentAudit: []overviewEvent{}}
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		const recentLimit = 5
		var approve, ownsImport bool
		result.CanManage = who.PlatformAdmin
		err := tx.QueryRow(ctx, `SELECT o.name,
			$3 OR EXISTS(SELECT 1 FROM device_groups dg WHERE dg.organization_id=o.id AND `+deviceGroupRead+`),
			$3 OR EXISTS(SELECT 1 FROM device_groups dg WHERE dg.organization_id=o.id AND `+deviceGroupApprove+`),
			EXISTS(SELECT 1 FROM tasks t WHERE t.organization_id=o.id AND t.kind='user_import' AND t.owner_id=$2),
			(SELECT count(*) FROM address_books b WHERE b.organization_id=o.id AND `+bookAccess+`>0)
			FROM organizations o WHERE o.id=$1`, who.Organization, who.ID, who.PlatformAdmin).
			Scan(&result.Organization, &result.CanReadDevices, &approve, &ownsImport, &result.Metrics.AddressBooks)
		if err != nil {
			return err
		}
		var counts sessionMetrics
		if err := tx.QueryRow(ctx, scopedSessionMetrics, who.Organization, who.ID, who.PlatformAdmin).
			Scan(&counts.Total, &counts.Active, &counts.Direct, &counts.Relayed); err != nil {
			return err
		}
		result.Metrics.ActiveSessions = counts.Active
		result.CanReadTasks = result.CanReadDevices || ownsImport
		result.CanReadWindowsReleases = a.store.WindowsReleases != nil && result.CanReadDevices
		result.CanReadUsers = who.PlatformAdmin
		if !result.CanReadUsers {
			result.CanReadUsers, err = canReadUsers(ctx, tx, who.ID)
			if err != nil {
				return err
			}
		}
		if result.CanReadUsers {
			err = tx.QueryRow(ctx, `SELECT count(*) FROM users u WHERE u.organization_id=$1 AND `+userAllowed,
				who.Organization, who.ID, "users.read", who.PlatformAdmin).Scan(&result.Metrics.Users)
			if err != nil {
				return err
			}
		}
		if result.CanReadDevices {
			var pending int64
			err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE (`+deviceConnectionState+`)='online'),
				count(*) FILTER (WHERE d.registration_state='pending' AND `+deviceGroupApprove+`)
				FROM devices d JOIN device_groups dg ON dg.id=d.group_id WHERE d.organization_id=$1 AND `+deviceGroupRead,
				who.Organization, who.ID, who.PlatformAdmin).Scan(&result.Metrics.OnlineDevices, &pending)
			if err != nil {
				return err
			}
			if approve {
				result.Metrics.PendingApprovals = &pending
			}
		}
		if who.PlatformAdmin {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM relay_nodes WHERE organization_id=$1 AND (`+
				relayStateSQL+`)='available'`, who.Organization).Scan(&result.Metrics.AvailableRelays); err != nil {
				return err
			}
			err = tx.QueryRow(ctx, `SELECT count(*) FROM task_targets tt JOIN tasks t ON t.id=tt.task_id
				WHERE t.organization_id=$1 AND (t.kind='user_import' OR ($3 AND t.kind='delivery') OR ($4 AND t.kind='windows_delivery'))
				AND tt.retryable AND tt.attempt_count<$2`, who.Organization, maxTaskAttempts, a.store.Delivery != nil,
				a.store.WindowsDelivery != nil).Scan(&result.Metrics.RetryableTargets)
			if err != nil {
				return err
			}
		}
		if result.CanReadTasks {
			rows, err := tx.Query(ctx, taskSummaries+taskSummaryGroup+` ORDER BY t.created_at DESC,t.id DESC LIMIT $4`,
				who.Organization, who.ID, who.PlatformAdmin, recentLimit)
			if err != nil {
				return err
			}
			result.RecentTasks, err = pgx.AppendRows(result.RecentTasks, rows, pgx.RowToStructByPos[taskSummary])
			if err != nil {
				return err
			}
		}
		result.CanReadAudit, _, err = auditCapabilities(ctx, tx, who)
		if err != nil || !result.CanReadAudit {
			return err
		}
		rows, err := auditQuery(ctx, tx, who, auditFilter{}, "", false, recentLimit)
		if err != nil {
			return err
		}
		for _, row := range rows {
			result.RecentAudit = append(result.RecentAudit, overviewEvent{row.ID, row.Action, row.OccurredAt, row.ReceiptState})
		}
		return nil
	})
	if err != nil {
		failure(w, "overview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
