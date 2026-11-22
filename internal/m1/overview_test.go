package m1

import (
	"net/http"
	"testing"
)

func TestOverviewScopesPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	createMember(t, h, admin, csrf)
	visible := deviceGroupFixture(t, h, admin, csrf, "概览授权组")
	hidden := deviceGroupFixture(t, h, admin, csrf, "概览其他组")
	in, _ := approvedDevice(t, h, admin, csrf, visible.ID, "10004025")
	out, _ := approvedDevice(t, h, admin, csrf, hidden.ID, "10004026")
	if _, err := store.DB.Exec(ctx, `UPDATE devices SET managed_heartbeat_at=clock_timestamp(),
		managed_heartbeat_epoch=device_key_epoch,managed_reported_build='overview-fixture' WHERE id IN ($1,$2)`, in.ID, out.ID); err != nil {
		t.Fatal(err)
	}
	grantPath := "/v1/admin/device-groups/" + visible.ID + "/grants"
	checkCode(t, request(h, "POST", grantPath, testOrigin, deviceGroupGrant{Username: "member"}, admin, csrf), 204, "")
	login := loginRequest(h, "member", testPassword)
	checkCode(t, login, 204, "")
	member := login.Result().Cookies()[0]
	read := func(cookie *http.Cookie) overviewSnapshot {
		reply := request(h, "GET", "/v1/overview", "", nil, cookie, "")
		checkCode(t, reply, 200, "online_devices")
		return responseValue[overviewSnapshot](t, reply.Body.Bytes())
	}
	full := read(admin)
	if full.Metrics.OnlineDevices == nil || *full.Metrics.OnlineDevices != 2 || len(full.RecentTasks) != 2 {
		t.Fatalf("organization overview: %+v", full)
	}
	scoped := read(member)
	if scoped.Metrics.OnlineDevices == nil || *scoped.Metrics.OnlineDevices != 1 || scoped.Metrics.PendingApprovals != nil ||
		scoped.Metrics.Users != nil || len(scoped.RecentTasks) != 1 || scoped.RecentTasks[0].DeviceID != in.ID || len(scoped.RecentAudit) != 0 {
		t.Fatalf("restricted overview: %+v", scoped)
	}
	checkCode(t, request(h, "POST", grantPath, testOrigin, map[string]any{"username": "member", "revoke": true}, admin, csrf), 204, "")
	reply := request(h, "GET", "/v1/overview", "", nil, member, "")
	checkCode(t, reply, 200, "address_books")
	revoked := responseValue[overviewSnapshot](t, reply.Body.Bytes())
	if revoked.CanReadDevices || revoked.CanReadTasks || revoked.Metrics.OnlineDevices != nil || len(revoked.RecentTasks) != 0 {
		t.Fatalf("overview exposed revoked scope: %+v", revoked)
	}
}
