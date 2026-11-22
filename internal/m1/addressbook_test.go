package m1

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddressBookScopesPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	for _, name := range []string{"reader", "editor", "peer"} {
		checkCode(t, request(h, "POST", "/v1/admin/users", testOrigin, map[string]string{"username": name, "password": testPassword}, admin, csrf), 201, name)
	}
	login := func(name string) *http.Cookie {
		t.Helper()
		res := loginRequest(h, name, testPassword)
		checkCode(t, res, 204, "")
		return res.Result().Cookies()[0]
	}
	reader, editor, peer := login("reader"), login("editor"), login("peer")
	post := func(who *http.Cookie, path string, value any) *httptest.ResponseRecorder {
		t.Helper()
		return request(h, "POST", path, testOrigin, value, who, csrfToken(who.Value))
	}
	get := func(who *http.Cookie, path string) *httptest.ResponseRecorder {
		t.Helper()
		return request(h, "GET", path, "", nil, who, "")
	}
	first := deviceGroupFixture(t, h, admin, csrf, "可以查看")
	second := deviceGroupFixture(t, h, admin, csrf, "限制区域")
	visible, _ := approvedDevice(t, h, admin, csrf, first.ID, "30100101")
	hidden, _ := approvedDevice(t, h, admin, csrf, second.ID, "30100102")
	setDeviceVisibility := func(name, group string, revoke bool) {
		t.Helper()
		checkCode(t, post(admin, "/v1/admin/device-groups/"+group+"/grants",
			deviceGroupGrant{Username: name, Revoke: revoke}), 204, "")
	}
	setDeviceVisibility("reader", first.ID, false)
	setDeviceVisibility("editor", first.ID, false)
	const books = "/v1/address-books"
	created := post(admin, books, map[string]string{"name": "日常设备"})
	checkCode(t, created, 201, "日常设备")
	book := responseValue[addressBook](t, created.Body.Bytes())
	path := books + "/" + book.ID
	checkCode(t, get(reader, path), 404, "address_book_not_found")
	checkCode(t, get(reader, books), 200, `"books":[]`)
	checkCode(t, get(nil, path), 401, "session_required")
	checkCode(t, post(admin, books, map[string]string{"name": "日常设备"}), 409, "address_book_name_in_use")
	private := post(peer, books, map[string]string{"name": "个人"})
	checkCode(t, private, 201, "个人")
	checkCode(t, get(admin, books+"/"+responseValue[addressBook](t, private.Body.Bytes()).ID), 404, "address_book_not_found")

	ent := entryInput{DeviceID: visible.ID, Alias: "办公室", Tags: []string{"运维"}, Note: "常用"}
	firstEntry := post(admin, path+"/entries", ent)
	checkCode(t, firstEntry, 201, visible.ID)
	item := responseValue[addressEntry](t, firstEntry.Body.Bytes())
	itemPath := path + "/entries/" + item.ID
	checkCode(t, post(admin, path+"/entries", ent), 409, "address_book_device_exists")
	secret := post(admin, path+"/entries", entryInput{DeviceID: hidden.ID, Alias: "隐藏别名", Tags: []string{"机密标签"}, Note: "隐藏备注"})
	checkCode(t, secret, 201, hidden.ID)
	secretID := responseValue[addressEntry](t, secret.Body.Bytes()).ID

	groupRes := post(admin, "/v1/admin/groups", map[string]string{"name": "共享组"})
	checkCode(t, groupRes, 201, "共享组")
	groupID := responseValue[group](t, groupRes.Body.Bytes()).ID
	membership := func(name string, remove bool) {
		t.Helper()
		checkCode(t, post(admin, "/v1/admin/groups/"+groupID+"/members", map[string]any{"username": name, "remove": remove}), 204, "")
	}
	membership("editor", false)
	// The wire shape forbids client-controlled share identifiers.
	shareInput := func(who *http.Cookie, kind, name string, permission int16) *httptest.ResponseRecorder {
		t.Helper()
		return post(who, path+"/shares", map[string]any{"kind": kind, "name": name, "permission": permission})
	}
	checkCode(t, post(admin, path+"/shares", addressGrant{ID: "unexpected", Kind: "user", Name: "reader", Permission: bookView}), 400, "invalid_request")
	checkCode(t, shareInput(admin, "user", "reader", bookView), 204, "")
	checkCode(t, shareInput(admin, "group", "共享组", bookEdit), 204, "")
	checkCode(t, get(reader, path), 200, `"permission":1`)
	checkCode(t, get(editor, path), 200, `"permission":2`)
	checkCode(t, get(reader, itemPath), 200, "办公室")

	list := get(reader, path+"/entries")
	checkCode(t, list, 200, visible.ID)
	for _, value := range []string{hidden.ID, secretID, "隐藏别名", "机密标签", "隐藏备注", hidden.RustDeskID} {
		if strings.Contains(list.Body.String(), value) {
			t.Fatalf("hidden entry field leaked: %s", value)
		}
	}
	checkCode(t, get(reader, path+"/entries/"+secretID), 404, "address_book_entry_not_found")
	checkCode(t, get(reader, path+"/shares"), 403, "forbidden")
	checkCode(t, shareInput(editor, "user", "peer", bookView), 403, "forbidden")
	edit := entryInput{Version: item.Version, Alias: "已编辑", Tags: []string{"新标签"}, Note: "已保存"}
	checkCode(t, post(reader, itemPath, edit), 403, "forbidden")
	checkCode(t, post(editor, itemPath, edit), 200, "已编辑")
	checkCode(t, post(editor, itemPath, edit), 409, "address_book_entry_stale")
	checkCode(t, post(editor, path+"/entries/"+secretID, edit), 404, "address_book_entry_not_found")
	checkCode(t, post(editor, path+"/entries", entryInput{DeviceID: hidden.ID}), 404, "device_not_found")

	if _, err := store.DB.Exec(ctx, "ALTER TABLE audit_events ADD CONSTRAINT block_share_audit CHECK (action<>'address_book.shared') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	checkCode(t, shareInput(admin, "user", "peer", bookManage), 500, "service_error")
	checkCode(t, get(peer, path), 404, "address_book_not_found")
	if _, err := store.DB.Exec(ctx, "ALTER TABLE audit_events DROP CONSTRAINT block_share_audit"); err != nil {
		t.Fatal(err)
	}
	checkCode(t, shareInput(admin, "user", "peer", bookManage), 204, "")
	checkCode(t, post(peer, path, map[string]string{"name": "协作设备"}), 200, "协作设备")
	checkCode(t, shareInput(peer, "user", "reader", bookView), 204, "")
	checkCode(t, shareInput(peer, "user", "admin", bookView), 409, "address_book_owner_share")
	checkCode(t, get(peer, path+"/entries"), 200, `"entries":[]`)
	checkCode(t, post(peer, itemPath, entryInput{Version: item.Version + 1, Alias: "不可修改"}), 404, "address_book_entry_not_found")

	if id, err := store.ResolveAddressBookDevice(ctx, reader.Value, book.ID, item.ID); err != nil || id != visible.ID {
		t.Fatalf("resolution: %s %v", id, err)
	}
	checkCode(t, post(admin, "/v1/admin/devices/"+visible.ID+"/lifecycle/disable", deviceChange{Version: visible.Version}), 200, "disabled")
	if _, err := store.ResolveAddressBookDevice(ctx, reader.Value, book.ID, item.ID); !errors.Is(err, errEntryInactive) {
		t.Fatalf("inactive device resolved: %v", err)
	}
	setDeviceVisibility("reader", first.ID, true)
	if _, err := store.ResolveAddressBookDevice(ctx, reader.Value, book.ID, item.ID); !errors.Is(err, errEntryMissing) {
		t.Fatalf("out-of-scope device resolved: %v", err)
	}
	membership("editor", true)
	checkCode(t, get(editor, path), 404, "address_book_not_found")
	checkCode(t, post(editor, itemPath, edit), 404, "address_book_not_found")
	shares := get(peer, path+"/shares")
	checkCode(t, shares, 200, "reader")
	var revokeID string
	for _, grant := range responseValue[grantPage](t, shares.Body.Bytes()).Shares {
		if grant.Name == "reader" {
			revokeID = grant.ID
		}
	}
	if revokeID == "" {
		t.Fatal("missing reader grant")
	}
	for range 2 {
		checkCode(t, post(peer, path+"/shares/"+revokeID+"/revoke", struct{}{}), 204, "")
	}
	checkCode(t, get(reader, path), 404, "address_book_not_found")
	if _, err := store.ResolveAddressBookDevice(ctx, reader.Value, book.ID, item.ID); !errors.Is(err, errBookMissing) {
		t.Fatalf("revoked address book resolved: %v", err)
	}
	checkCode(t, get(reader, books), 200, `"books":[]`)
	checkCode(t, post(admin, itemPath+"/remove", map[string]int64{"version": item.Version + 1}), 204, "")
	checkCode(t, get(admin, itemPath), 404, "address_book_entry_not_found")
	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events a JOIN outbox o ON o.event_id=a.id
		WHERE a.resource_id=$1 AND a.action='address_book.share_revoked' AND a.reason LIKE $2`, book.ID, revokeID+" user %").Scan(&count); err != nil || count != 1 {
		t.Fatalf("share revocation audit count %d: %v", count, err)
	}
}
