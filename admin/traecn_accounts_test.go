package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestParseTraeCNRefreshTokensSplitsAndDeduplicates(t *testing.T) {
	t.Parallel()
	got, err := parseTraeCNRefreshTokens(json.RawMessage(`" first\nsecond,first "`), "third\r\nsecond")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokens = %#v, want %#v", got, want)
		}
	}
	array, err := parseTraeCNRefreshTokens(json.RawMessage(`["a","b","a"]`), "")
	if err != nil || len(array) != 2 || array[0] != "a" || array[1] != "b" {
		t.Fatalf("array tokens = %#v, err=%v", array, err)
	}
}

func TestResolveTraeCNGroupIDsEnforcesChannelBeforeImport(t *testing.T) {
	handler, db, _, codexGroupID := newImportGroupsTestHandler(t)
	ctx := context.Background()
	traeGroupID, err := db.CreateAccountGroup(ctx, "trae", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	channel := database.AccountGroupChannelTraeCN
	if err := db.UpdateAccountGroup(ctx, traeGroupID, nil, nil, nil, &database.UpdateAccountGroupOpts{Channel: &channel}); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.resolveTraeCNGroupIDs(ctx, json.RawMessage(`[`+itoa(codexGroupID)+`]`)); err == nil {
		t.Fatal("Codex group must be rejected for Trae CN accounts")
	}
	ids, err := handler.resolveTraeCNGroupIDs(ctx, json.RawMessage(`[`+itoa(traeGroupID)+`,`+itoa(traeGroupID)+`]`))
	if err != nil || len(ids) != 1 || ids[0] != traeGroupID {
		t.Fatalf("Trae group resolution = %v, %v", ids, err)
	}
}

// Import reserves the RT in the database before ExchangeToken so the newly
// allocated durable ID is available as the Resin lease identity on the very
// first provider request. This prevents the import exchange from using a
// transient "0" lease and then switching egress for inference.
func TestAddTraeCNAccountFirstExchangeUsesInsertedIDForResin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousDecorator := auth.ResinRequestDecorator
	t.Cleanup(func() { auth.ResinRequestDecorator = previousDecorator })

	type decoratedRequest struct {
		targetURL string
		accountID string
	}
	decorated := make(chan decoratedRequest, 1)
	type resinRequest struct {
		path         string
		resinAccount string
	}
	received := make(chan resinRequest, 1)
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- resinRequest{path: r.URL.Path, resinAccount: r.Header.Get("X-Resin-Account")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"import-at","refreshToken":"import-rt-rotated","expiresIn":3600,"userId":"trae-user"}`))
	}))
	defer resin.Close()
	auth.ResinRequestDecorator = func(targetURL, accountID string) string {
		decorated <- decoratedRequest{targetURL: targetURL, accountID: accountID}
		return resin.URL + "/resin/exchange"
	}

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn", strings.NewReader(`{
		"name":"resin-import",
		"refresh_token":"import-rt",
		"host":"https://trae-origin.invalid",
		"proxy_url":"http://127.0.0.1:1"
	}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.AddTraeCNAccounts(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items []struct {
			ID      int64  `json:"id"`
			OK      bool   `json:"ok"`
			Warning string `json:"warning"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].ID <= 0 || !response.Items[0].OK {
		t.Fatalf("unexpected import result: %+v; body=%s", response.Items, recorder.Body.String())
	}
	wantAccountID := strconv.FormatInt(response.Items[0].ID, 10)

	var decoration decoratedRequest
	select {
	case decoration = <-decorated:
	case <-time.After(2 * time.Second):
		t.Fatal("first ExchangeToken was not decorated for Resin")
	}
	if decoration.targetURL != "https://trae-origin.invalid"+auth.TraeCNExchangePath {
		t.Fatalf("decorated target = %q", decoration.targetURL)
	}
	if decoration.accountID != wantAccountID {
		t.Fatalf("decorator account = %q, want inserted ID %q", decoration.accountID, wantAccountID)
	}

	var request resinRequest
	select {
	case request = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("first ExchangeToken did not reach Resin")
	}
	if request.path != "/resin/exchange" {
		t.Fatalf("Resin request path = %q, want /resin/exchange", request.path)
	}
	if request.resinAccount != wantAccountID {
		t.Fatalf("X-Resin-Account = %q, want inserted ID %q", request.resinAccount, wantAccountID)
	}

	row, err := db.GetAccountByID(context.Background(), response.Items[0].ID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.GetCredential("access_token") != "import-at" || row.GetCredential("refresh_token") != "import-rt-rotated" {
		t.Fatalf("persisted credentials = %q/%q", row.GetCredential("access_token"), row.GetCredential("refresh_token"))
	}
}

func TestSyncTraeCNUpstreamModelsPersistsCatalogAndLegacyAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			// Direct Trae hosts do not expose the wrapper's OpenAI model endpoint.
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models/detail":
			// This fixture emulates a raw Trae host; the wrapper-compatible
			// detail route is intentionally unavailable and should fall back.
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/ide/v1/get_detail_param":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
  "config_info_list": [
    {"config_name":"DeepSeek-V4-Pro","usage":"chat_completion","config_switch":true},
    {"config_name":"glm-5.2_advisor_doubao","usage":"chat_completion","config_switch":true},
    {"config_name":"summary","usage":"summary","config_switch":true}
  ]
}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "trae-sync", "trae", auth.UpstreamTraeCN, map[string]any{
		"upstream_type": auth.UpstreamTraeCN,
		"access_token":  "AT",
		"refresh_token": "RT",
		"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"traecn_host":   provider.URL,
		// This emulates a row created before the dedicated allowlist field.
		// 内置别名表已删除，允许清单必须写 provider 目录里的名字。
		"models": []string{"deepseek-v4-pro"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(id, 10)+"/models/sync-upstream", nil)
	handler.SyncAccountUpstreamModels(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Models          []string `json:"models"`
		EffectiveModels []string `json:"effective_models"`
		SyncedAt        string   `json:"synced_at"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !containsStringFold(response.Models, "deepseek-v4-pro") || !containsStringFold(response.Models, "auto") {
		t.Fatalf("synced catalog = %#v", response.Models)
	}
	if len(response.EffectiveModels) != 1 || !containsStringFold(response.EffectiveModels, "deepseek-v4-pro") {
		t.Fatalf("effective models = %#v, want legacy allowlist intersection", response.EffectiveModels)
	}
	if response.SyncedAt == "" {
		t.Fatal("sync response omitted synced_at")
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStringFold(row.GetCredentialStringSlice(auth.TraeCNUpstreamModelsCredentialKey), "deepseek-v4-pro") {
		t.Fatalf("persisted upstream catalog = %#v", row.GetCredentialStringSlice(auth.TraeCNUpstreamModelsCredentialKey))
	}
	if allowlist := row.GetCredentialStringSlice(auth.TraeCNModelAllowlistCredentialKey); len(allowlist) != 1 || !containsStringFold(allowlist, "deepseek-v4-pro") {
		t.Fatalf("persisted allowlist = %#v", allowlist)
	}
	runtime := store.FindByID(id)
	if runtime == nil || !runtime.TraeCNSupportsModel("deepseek-v4-pro") || runtime.TraeCNSupportsModel("glm-5.2") {
		t.Fatalf("runtime catalog/allowlist mismatch: %#v", runtime)
	}
}

func containsStringFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}
