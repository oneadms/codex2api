package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestParseTraeCNImportJSONAcceptsExportPayloadAndAliases(t *testing.T) {
	t.Parallel()
	exported := `{
  "version": 1,
  "exported_at": "2026-09-11T00:00:00Z",
  "channel": "traecn",
  "accounts": [
    {"name":"a1","email":"a1@example.com","host":"https://trae-api-cn.mchost.guru","refresh_token":"rt-1","access_token":"at-1","user_id":"u-1"},
    {"name":"a2","refreshToken":"rt-2","userId":"u-2"}
  ]
}`
	items, err := parseTraeCNImportJSON(exported)
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %+v, err=%v", items, err)
	}
	if items[0].RefreshToken != "rt-1" || items[0].Email != "a1@example.com" {
		t.Fatalf("first item = %+v", items[0])
	}
	if items[1].RefreshToken != "rt-2" || items[1].UserID != "u-2" {
		t.Fatalf("alias item = %+v", items[1])
	}

	bare := `[{"rt":"rt-3"},{"refresh_token":"rt-4"}]`
	items, err = parseTraeCNImportJSON(bare)
	if err != nil || len(items) != 2 || items[0].RefreshToken != "rt-3" || items[1].RefreshToken != "rt-4" {
		t.Fatalf("bare array = %+v, err=%v", items, err)
	}

	single := `{"name":"solo","RefreshToken":"rt-5","accessToken":"at-5"}`
	items, err = parseTraeCNImportJSON(single)
	if err != nil || len(items) != 1 || items[0].RefreshToken != "rt-5" || items[0].AccessToken != "at-5" {
		t.Fatalf("single object = %+v, err=%v", items, err)
	}

	for _, invalid := range []string{``, `{"name":"no-token"}`, `not json`} {
		if _, err := parseTraeCNImportJSON(invalid); err == nil {
			t.Errorf("input %q should fail", invalid)
		}
	}
}

func TestTraeCNImportJSONCreatesAccountsAndSkipsDuplicates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/import-json", strings.NewReader(`{
		"json": "{\"accounts\":[{\"name\":\"json-1\",\"email\":\"j1@example.com\",\"refresh_token\":\"json-rt-1\",\"access_token\":\"json-at-1\",\"user_id\":\"json-u-1\",\"host\":\"https://trae-api-cn.mchost.guru\"},{\"name\":\"json-2\",\"refresh_token\":\"json-rt-2\"}]}"
	}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.TraeCNImportJSON(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success int `json:"success"`
		Failed  int `json:"failed"`
		Items   []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Success != 2 || response.Failed != 0 {
		t.Fatalf("response = %+v body=%s", response, recorder.Body.String())
	}
	row, err := db.GetAccountByID(context.Background(), response.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("refresh_token") != "json-rt-1" || row.GetCredential("access_token") != "json-at-1" || row.GetCredential("email") != "j1@example.com" {
		t.Fatalf("credentials = %+v", row.Credentials)
	}

	// 重复导入同一个 RT 必须被拒绝，而不是静默创建第二个账号。
	recorder = httptest.NewRecorder()
	ginContext, _ = gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/import-json", strings.NewReader(`{"json":"[{\"refresh_token\":\"json-rt-1\"}]"}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.TraeCNImportJSON(ginContext)
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Success != 0 || response.Failed != 1 {
		t.Fatalf("duplicate import response = %+v body=%s", response, recorder.Body.String())
	}
}

func TestExportTraeCNAccountsRoundTripsThroughImport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	ctx := context.Background()

	if _, err := db.InsertAccountWithUpstream(ctx, "trae-export", "trae", auth.UpstreamTraeCN, map[string]any{
		"upstream_type":  auth.UpstreamTraeCN,
		"access_token":   "export-at",
		"refresh_token":  "export-rt",
		"traecn_host":    "https://trae-api-cn.mchost.guru",
		"email":          "export@example.com",
		"traecn_user_id": "export-u",
	}, ""); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/traecn/export", nil)
	handler.ExportTraeCNAccounts(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if disposition := recorder.Header().Get("Content-Disposition"); !strings.Contains(disposition, "traecn-accounts-") {
		t.Fatalf("content-disposition = %q", disposition)
	}
	var payload TraeCNExportPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Channel != auth.UpstreamTraeCN || len(payload.Accounts) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	account := payload.Accounts[0]
	if account.RefreshToken != "export-rt" || account.AccessToken != "export-at" || account.Email != "export@example.com" {
		t.Fatalf("exported account = %+v", account)
	}

	items, err := parseTraeCNImportJSON(string(recorder.Body.Bytes()))
	if err != nil || len(items) != 1 || items[0].RefreshToken != "export-rt" {
		t.Fatalf("round trip = %+v err=%v", items, err)
	}
}

// 整条 OAuth 链路：start -> 公开回调（自动兑换）-> status -> claim 建号。
func TestTraeCNOAuthEndToEndCreatesAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case auth.TraeCNOAuthGuidancePath:
			_, _ = w.Write([]byte(`{"Result":{"LoginHost":"http://` + r.Host + `"}}`))
		case auth.TraeCNOAuthAuthCodeExchangePth:
			_, _ = w.Write([]byte(`{"Result":{"Token":"e2e-at","RefreshToken":"e2e-rt","ExpiresAt":4102444800000,"UserID":"e2e-user"}}`))
		case auth.TraeCNOAuthUserInfoPath:
			_, _ = w.Write([]byte(`{"Result":{"NonPlainTextEmail":"e2e@example.com","UserID":"e2e-user"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	t.Setenv("TRAECN_OAUTH_GUIDANCE_HOSTS", fixture.URL)
	t.Setenv("TRAECN_OAUTH_AUTH_HOST", fixture.URL)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}

	startRecorder := httptest.NewRecorder()
	startContext, _ := gin.CreateTestContext(startRecorder)
	startContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/oauth/start", strings.NewReader(`{"name":"oauth-account","callback_base":"https://gw.example.com"}`))
	startContext.Request.Header.Set("Content-Type", "application/json")
	startContext.Request.Host = "gw.example.com"
	handler.StartTraeCNOAuth(startContext)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", startRecorder.Code, startRecorder.Body.String())
	}
	var start struct {
		LoginID     string `json:"login_id"`
		LoginTrace  string `json:"login_trace_id"`
		CallbackURL string `json:"callback_url"`
	}
	if err := json.Unmarshal(startRecorder.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	// 管理台来源是 gw.example.com（非回环），回调落到默认回环端口：Trae 只认
	// http://127.0.0.1:<port>/authorize，远端部署靠用户把地址栏链接粘回来。
	if start.LoginID == "" || start.CallbackURL != "http://127.0.0.1:53999"+auth.TraeCNOAuthCallbackPath {
		t.Fatalf("start = %+v", start)
	}

	// 浏览器被重定向回公开回调端点（没有管理密钥）。
	callbackRecorder := httptest.NewRecorder()
	callbackContext, _ := gin.CreateTestContext(callbackRecorder)
	callbackContext.Request = httptest.NewRequest(http.MethodGet,
		"/api/traecn/oauth/callback?login_trace_id="+start.LoginTrace+"&authCode=e2e-code", nil)
	handler.TraeCNOAuthCallback(callbackContext)
	if callbackRecorder.Code != http.StatusOK || !strings.Contains(callbackRecorder.Body.String(), "授权成功") {
		t.Fatalf("callback status = %d body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}

	statusRecorder := httptest.NewRecorder()
	statusContext, _ := gin.CreateTestContext(statusRecorder)
	statusContext.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/traecn/oauth/status?login_id="+start.LoginID, nil)
	handler.GetTraeCNOAuthStatus(statusContext)
	var status struct {
		State   string `json:"state"`
		Account *struct {
			Email   string `json:"email"`
			UserID  string `json:"user_id"`
			Warning string `json:"warning"`
		} `json:"account"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || status.Account == nil || status.Account.Email != "e2e@example.com" || status.Account.UserID != "e2e-user" {
		t.Fatalf("status = %+v body=%s", status, statusRecorder.Body.String())
	}
	// 状态接口不回传凭据明文（建号走服务端会话）。
	if strings.Contains(statusRecorder.Body.String(), "e2e-rt") || strings.Contains(statusRecorder.Body.String(), "e2e-at") {
		t.Fatalf("status leaked credentials: %s", statusRecorder.Body.String())
	}

	claimRecorder := httptest.NewRecorder()
	claimContext, _ := gin.CreateTestContext(claimRecorder)
	claimContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/oauth/claim",
		strings.NewReader(`{"login_id":"`+start.LoginID+`","name":"oauth-account"}`))
	claimContext.Request.Header.Set("Content-Type", "application/json")
	handler.ClaimTraeCNOAuthAccount(claimContext)
	if claimRecorder.Code != http.StatusOK {
		t.Fatalf("claim status = %d body=%s", claimRecorder.Code, claimRecorder.Body.String())
	}
	var claim struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(claimRecorder.Body.Bytes(), &claim); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("refresh_token") != "e2e-rt" || row.GetCredential("access_token") != "e2e-at" || row.GetCredential("email") != "e2e@example.com" {
		t.Fatalf("claimed credentials = %+v", row.Credentials)
	}
	if !strings.EqualFold(row.GetCredential("upstream_type"), auth.UpstreamTraeCN) {
		t.Fatalf("upstream type = %q", row.GetCredential("upstream_type"))
	}
	// OAuth 建号必须把授权会话里的设备码绑定下来。
	if row.GetCredential(auth.TraeCNMachineIDCredentialKey) == "" || row.GetCredential(auth.TraeCNDeviceIDCredentialKey) == "" {
		t.Fatalf("oauth account has no bound device code: %+v", row.Credentials)
	}

	// 二次 claim 不能建出第二个账号。
	claimRecorder = httptest.NewRecorder()
	claimContext, _ = gin.CreateTestContext(claimRecorder)
	claimContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/oauth/claim",
		strings.NewReader(`{"login_id":"`+start.LoginID+`","name":"oauth-account"}`))
	claimContext.Request.Header.Set("Content-Type", "application/json")
	handler.ClaimTraeCNOAuthAccount(claimContext)
	if claimRecorder.Code != http.StatusConflict {
		t.Fatalf("second claim status = %d body=%s", claimRecorder.Code, claimRecorder.Body.String())
	}
}

func TestTraeCNOAuthCallbackReportsUnknownSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/traecn/oauth/callback?login_trace_id=nope", nil)
	handler.TraeCNOAuthCallback(ginContext)
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "会话已过期") {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestTraeCNCallbackBaseFallsBackToRequestOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "http://internal:8080/api/admin/accounts/traecn/oauth/start", nil)
	ginContext.Request.Header.Set("X-Forwarded-Proto", "https")
	ginContext.Request.Header.Set("X-Forwarded-Host", "gw.example.com")
	if got := traeCNCallbackBase(ginContext, ""); got != "https://gw.example.com" {
		t.Fatalf("callback base = %q", got)
	}
	if got := traeCNCallbackBase(ginContext, "https://other.example.com:9443"); got != "https://other.example.com:9443" {
		t.Fatalf("explicit callback base = %q", got)
	}
	// 最终回调地址永远收敛到 Trae 唯一接受的写法，端口只在管理台来源为回环时复用。
	loopback := httptest.NewRecorder()
	loopbackContext, _ := gin.CreateTestContext(loopback)
	loopbackContext.Request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/admin/accounts/traecn/oauth/start", nil)
	callback, err := auth.TraeCNOAuthCallbackURL(traeCNCallbackBase(loopbackContext, ""))
	if err != nil || callback != "http://127.0.0.1:8080"+auth.TraeCNOAuthCallbackPath {
		t.Fatalf("loopback callback = %q, err=%v", callback, err)
	}
	remote, err := auth.TraeCNOAuthCallbackURL(traeCNCallbackBase(ginContext, ""))
	if err != nil || !strings.HasPrefix(remote, "http://127.0.0.1:") || !strings.HasSuffix(remote, auth.TraeCNOAuthCallbackPath) {
		t.Fatalf("remote callback = %q, err=%v", remote, err)
	}
}

// 导出会把设备码一起带走（迁移到另一台网关时不能换设备）。导入时的判定维度是
// Trae 账号：不同账号撞到同一个设备码要重新分配，同一个账号沿用原码是对的。
func TestTraeCNImportReassignsDeviceCodeOwnedByAnotherAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	ctx := context.Background()

	existing := auth.NewTraeCNDeviceIdentity()
	existingID, err := db.InsertAccountWithUpstream(ctx, "trae-existing", "trae", auth.UpstreamTraeCN, map[string]any{
		"upstream_type":                   auth.UpstreamTraeCN,
		"refresh_token":                   "existing-rt",
		"traecn_user_id":                  "user-a",
		auth.TraeCNMachineIDCredentialKey: existing.MachineID,
		auth.TraeCNDeviceIDCredentialKey:  existing.DeviceID,
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{"accounts": []any{
		// 不同 Trae 账号 + 已有设备码 -> 必须重新分配
		map[string]any{
			"name":          "other-account",
			"refresh_token": "other-rt",
			"user_id":       "user-b",
			"machine_id":    existing.MachineID,
			"device_id":     existing.DeviceID,
		},
		// 完全相同的 Trae 账号 + 已有设备码 -> 沿用原码，只提示重复添加
		map[string]any{
			"name":          "same-account",
			"refresh_token": "same-rt",
			"user_id":       "user-a",
			"machine_id":    existing.MachineID,
			"device_id":     existing.DeviceID,
		},
		// 没有 user id，判断不了是不是同一个账号 -> 按冲突处理
		map[string]any{
			"name":          "unknown-account",
			"refresh_token": "unknown-rt",
			"machine_id":    existing.MachineID,
			"device_id":     existing.DeviceID,
		},
	}}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/import-json",
		strings.NewReader(string(traeCNMustJSON(t, map[string]any{"json": string(traeCNMustJSON(t, payload))}))))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.TraeCNImportJSON(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success int `json:"success"`
		Items   []struct {
			ID      int64  `json:"id"`
			Warning string `json:"warning"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Success != 3 {
		t.Fatalf("response = %+v body=%s", response, recorder.Body.String())
	}
	machineOf := func(id int64) string {
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return row.GetCredential(auth.TraeCNMachineIDCredentialKey)
	}

	if got := machineOf(response.Items[0].ID); got == existing.MachineID {
		t.Fatalf("a different Trae account reused the device code of account #%d", existingID)
	}
	if !strings.Contains(response.Items[0].Warning, "另一个 Trae 账号") {
		t.Fatalf("warning = %q, want a device-code conflict notice", response.Items[0].Warning)
	}
	if got := machineOf(response.Items[1].ID); got != existing.MachineID {
		t.Fatalf("the same Trae account must keep its own device code, got %q", got)
	}
	if strings.Contains(response.Items[1].Warning, "另一个 Trae 账号") {
		t.Fatalf("same account should not be reported as a conflict: %q", response.Items[1].Warning)
	}
	if !strings.Contains(response.Items[1].Warning, "已在号池中") {
		t.Fatalf("warning = %q, want a duplicate Trae-account notice", response.Items[1].Warning)
	}
	if got := machineOf(response.Items[2].ID); got == existing.MachineID {
		t.Fatalf("an entry without user id must not silently share the device code")
	}
}

// 导出的设备码在空号池里必须原样保留：迁移后不能换设备。
func TestTraeCNImportKeepsDeviceCodeWhenNotInUse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}

	identity := auth.NewTraeCNDeviceIdentity()
	payload := map[string]any{"accounts": []any{map[string]any{
		"name":          "trae-moved",
		"refresh_token": "moved-rt",
		"machine_id":    identity.MachineID,
		"device_id":     identity.DeviceID,
	}}}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/import-json",
		strings.NewReader(string(traeCNMustJSON(t, map[string]any{"json": string(traeCNMustJSON(t, payload))}))))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.TraeCNImportJSON(ginContext)

	var response struct {
		Items []struct {
			ID      int64  `json:"id"`
			Warning string `json:"warning"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), response.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential(auth.TraeCNMachineIDCredentialKey) != identity.MachineID ||
		row.GetCredential(auth.TraeCNDeviceIDCredentialKey) != identity.DeviceID {
		t.Fatalf("migration changed the device code: %q/%q, want %q/%q",
			row.GetCredential(auth.TraeCNMachineIDCredentialKey), row.GetCredential(auth.TraeCNDeviceIDCredentialKey),
			identity.MachineID, identity.DeviceID)
	}
	if strings.Contains(response.Items[0].Warning, "设备码已被账号") {
		t.Fatalf("unexpected conflict warning for a free device code: %q", response.Items[0].Warning)
	}
}

func traeCNMustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// RT 粘贴导入也要给每个账号绑定各自的设备码（一账号一码）。
func TestTraeCNAddAccountsBindsDistinctDeviceCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	// 上游不可达：RT 会原样保存，这里只关心设备码绑定。
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn",
		strings.NewReader(`{"name":"rt-bind","refresh_tokens":["rt-bind-1","rt-bind-2"],"host":"https://127.0.0.1:1"}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.AddTraeCNAccounts(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items []struct {
			ID     int64 `json:"id"`
			Stored bool  `json:"stored"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 {
		t.Fatalf("items = %+v body=%s", response.Items, recorder.Body.String())
	}
	seen := map[string]bool{}
	for _, item := range response.Items {
		row, err := db.GetAccountByID(context.Background(), item.ID)
		if err != nil {
			t.Fatal(err)
		}
		machineID := row.GetCredential(auth.TraeCNMachineIDCredentialKey)
		deviceID := row.GetCredential(auth.TraeCNDeviceIDCredentialKey)
		if len(machineID) != 64 || len(deviceID) != auth.TraeCNDeviceIDDigits {
			t.Fatalf("account %d device code = %q/%q", item.ID, machineID, deviceID)
		}
		if seen[machineID] || seen[deviceID] {
			t.Fatalf("account %d shares a device code with another import", item.ID)
		}
		seen[machineID] = true
		seen[deviceID] = true
		if row.GetCredential(auth.TraeCNDeviceBoundAtCredentialKey) == "" {
			t.Fatalf("account %d has no device bound_at", item.ID)
		}
	}
}

// OAuth 新增路由必须注册成功，且公开回调与 /accounts/:id/... 同层不冲突。
func TestTraeCNOAuthRoutesRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	router := gin.New()
	handler.RegisterRoutes(router)

	want := map[string]bool{
		"POST /api/admin/accounts/traecn/oauth/start":    false,
		"GET /api/admin/accounts/traecn/oauth/status":    false,
		"POST /api/admin/accounts/traecn/oauth/complete": false,
		"POST /api/admin/accounts/traecn/oauth/claim":    false,
		"POST /api/admin/accounts/traecn/import-json":    false,
		"GET /api/admin/accounts/traecn/export":          false,
		"GET /authorize":                                 false,
		"GET /api/traecn/oauth/callback":                 false,
	}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("route %s is not registered", key)
		}
	}
}
