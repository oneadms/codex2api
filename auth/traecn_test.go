package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestTraeCNSupportsModelUsesDefaultCatalogWhenModelsAreUnset(t *testing.T) {
	t.Parallel()
	account := &Account{
		UpstreamType: UpstreamTraeCN,
		AccessToken:  "at",
		RefreshToken: "rt",
	}
	if !account.TraeCNSupportsModel("glm-5.3-flash") {
		t.Fatal("unset Models should use the built-in Trae CN catalog")
	}
	if account.TraeCNSupportsModel("gpt-5.6-sol") {
		t.Fatal("unset Models must not act as a wildcard for Codex-only models")
	}
}

func TestTraeCNSupportsModelHonorsExplicitModelsAllowlist(t *testing.T) {
	t.Parallel()
	account := &Account{
		UpstreamType: UpstreamTraeCN,
		AccessToken:  "at",
		RefreshToken: "rt",
		Models:       []string{"glm-5.2"},
	}
	if !account.TraeCNSupportsModel("glm-5.2") {
		t.Fatal("declared Trae model should be accepted")
	}
	if account.TraeCNSupportsModel("kimi-k3") {
		t.Fatal("explicit Models list should narrow the Trae catalog")
	}
	if account.TraeCNSupportsModel("glm-5.3-flash") {
		t.Fatal("a distinct catalog model must not bypass the explicit glm-5.2 allowlist")
	}
}

// 内置别名表删除后，只有大小写/分隔符差异仍算同一个模型；glm-5.2 不再等于
// glm-5.3，改名由管理员的 TRAECN 模型映射负责。
func TestTraeCNSupportsModelMatchesCatalogNamesAcrossNamingStyles(t *testing.T) {
	t.Parallel()
	account := &Account{
		UpstreamType:               UpstreamTraeCN,
		AccessToken:                "at",
		RefreshToken:               "rt",
		TraeCNUpstreamModelCatalog: []string{"DeepSeek-V4-Pro", "Doubao_1_6", "auto"},
	}
	for _, model := range []string{"deepseek-v4-pro", "DeepSeek-V4-Pro", "doubao-1-6", "auto"} {
		if !account.TraeCNSupportsModel(model) {
			t.Errorf("catalog model %q should be routable", model)
		}
	}
	for _, model := range []string{"deepseek-v3", "gpt-5.6-sol", "claude-opus-4-7"} {
		if account.TraeCNSupportsModel(model) {
			t.Errorf("model %q must not match a Trae catalog without an explicit mapping", model)
		}
	}
}

func TestTraeCNAllowlistIntersectsByNamingStyle(t *testing.T) {
	t.Parallel()
	account := &Account{
		UpstreamType:               UpstreamTraeCN,
		AccessToken:                "at",
		RefreshToken:               "rt",
		TraeCNUpstreamModelCatalog: []string{"DeepSeek-V4-Pro", "glm-5.2"},
	}
	// 允许清单写目录风格的名字，provider 目录写成 DeepSeek-V4-Pro，仍要命中。
	got := account.TraeCNModelsForAllowlist([]string{"deepseek-v4-pro"})
	if len(got) != 1 || got[0] != "DeepSeek-V4-Pro" {
		t.Fatalf("allowlist intersection = %#v", got)
	}
	// 别名表已删除：旧兼容 ID 不再等价于别的模型。
	if got := account.TraeCNModelsForAllowlist([]string{"deepseek-v3"}); len(got) != 0 {
		t.Fatalf("removed alias still matched a catalog model: %#v", got)
	}
}

func TestApplyTraeCNConfigCanClearLegacyAllowlist(t *testing.T) {
	t.Parallel()
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	account := &Account{
		DBID:         77,
		UpstreamType: UpstreamTraeCN,
		AccessToken:  "at",
		RefreshToken: "rt",
		Models:       []string{"deepseek-v3"},
	}
	store.AddAccount(account)
	if !store.ApplyTraeCNConfig(account.DBID, TraeCNDefaultHost, nil, "") {
		t.Fatal("ApplyTraeCNConfig returned false")
	}
	if got := account.TraeCNEffectiveModels(); len(got) != len(TraeCNDefaultModelIDs()) {
		t.Fatalf("cleared allowlist still narrowed catalog: got %d models, want %d (%#v)", len(got), len(TraeCNDefaultModelIDs()), got)
	}
}

func TestTraeCNExplicitEmptyAllowlistSurvivesFirstCatalogSync(t *testing.T) {
	t.Parallel()
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	account := &Account{
		DBID:         78,
		UpstreamType: UpstreamTraeCN,
		AccessToken:  "at",
		RefreshToken: "rt",
		// Legacy rows may still have the old effective projection.
		Models: []string{"deepseek-v3"},
	}
	store.AddAccount(account)
	if !store.ApplyTraeCNConfig(account.DBID, TraeCNDefaultHost, nil, "") {
		t.Fatal("ApplyTraeCNConfig returned false")
	}
	if configured := account.TraeCNConfiguredModelAllowlist(); len(configured) != 0 {
		t.Fatalf("explicit empty allowlist = %#v, want empty", configured)
	}
	if !store.ApplyTraeCNUpstreamModels(account.DBID, []string{"new-provider-model"}, time.Now().UTC()) {
		t.Fatal("ApplyTraeCNUpstreamModels returned false")
	}
	if got := account.TraeCNEffectiveModels(); len(got) != 1 || got[0] != "new-provider-model" {
		t.Fatalf("catalog after explicit clear = %#v, want new-provider-model", got)
	}
}

func TestNormalizeTraeCNHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default", want: TraeCNDefaultHost},
		{name: "trim origin", input: "  HTTPS://Example.COM///  ", want: "https://Example.COM"},
		{name: "http local", input: "http://127.0.0.1:8080/", want: "http://127.0.0.1:8080"},
		{name: "reject path", input: "https://example.com/api", wantErr: true},
		{name: "reject query", input: "https://example.com/?token=x", wantErr: true},
		{name: "reject fragment", input: "https://example.com/#x", wantErr: true},
		{name: "reject userinfo", input: "https://user@example.com", wantErr: true},
		{name: "reject scheme", input: "ftp://example.com", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeTraeCNHost(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeTraeCNHost(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("NormalizeTraeCNHost(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeTraeTokenResponseNestedAndMissingFields(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"data":{"result":{"token_data":{"access_token":"AT","refresh_token":"RT2","expires_in":"3600","user_id":"user-1"}}}}`)
	before := time.Now().UTC().Add(59 * time.Minute)
	token, err := normalizeTraeTokenResponse(raw)
	if err != nil {
		t.Fatalf("normalizeTraeTokenResponse() error = %v", err)
	}
	if token.AccessToken != "AT" || token.RefreshToken != "RT2" || token.UserID != "user-1" {
		t.Fatalf("unexpected token: %+v", token)
	}
	if token.ExpiresAt.Before(before) || token.ExpiresAt.After(time.Now().UTC().Add(61*time.Minute)) {
		t.Fatalf("expires_at = %s, want about one hour from now", token.ExpiresAt)
	}
	if strings.Contains(token.RefreshToken, "<nil>") || strings.Contains(token.UserID, "<nil>") {
		t.Fatalf("missing fields were stringified: %+v", token)
	}
}

func TestNormalizeTraeTokenResponseUsesJWTExpiry(t *testing.T) {
	t.Parallel()
	want := time.Now().UTC().Truncate(time.Second).Add(45 * time.Minute)
	payload, _ := json.Marshal(map[string]any{"exp": want.Unix()})
	jwt := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	raw, _ := json.Marshal(map[string]any{"response": map[string]any{"token": jwt}})
	token, err := normalizeTraeTokenResponse(raw)
	if err != nil {
		t.Fatalf("normalizeTraeTokenResponse() error = %v", err)
	}
	if !token.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %s, want %s", token.ExpiresAt, want)
	}
}

func TestNormalizeTraeTokenResponseCurrentTraeEnvelope(t *testing.T) {
	t.Parallel()
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	claims, err := json.Marshal(map[string]any{
		"data": map[string]any{"id": "uid-from-jwt"},
		"exp":  expiresAt.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	accessToken := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	raw, err := json.Marshal(map[string]any{
		"ResponseMetadata": map[string]any{"Action": ""},
		"Result": map[string]any{
			"RefreshToken":    "rotated-rt",
			"Token":           accessToken,
			"TokenExpireAt":   expiresAt.UnixMilli(),
			"RefreshExpireAt": expiresAt.Add(30 * 24 * time.Hour).UnixMilli(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := normalizeTraeTokenResponse(raw)
	if err != nil {
		t.Fatalf("normalizeTraeTokenResponse() error = %v", err)
	}
	if token.AccessToken != accessToken || token.RefreshToken != "rotated-rt" || token.UserID != "uid-from-jwt" {
		t.Fatalf("unexpected token: %+v", token)
	}
	if !token.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expires_at = %s, want %s", token.ExpiresAt, expiresAt)
	}
	if token.RefreshExpiresAt.IsZero() {
		t.Fatal("refresh expiry was not parsed")
	}
}

func TestTraeCNExchangeHostUsesCurrentAuthDomainForDefaultAgentHost(t *testing.T) {
	t.Parallel()
	if got := traeCNExchangeHost(TraeCNDefaultHost); got != TraeCNAuthHost {
		t.Fatalf("default exchange host = %q, want %q", got, TraeCNAuthHost)
	}
	if got := traeCNExchangeHost("https://TRAE-API-CN.MCHOST.GURU/"); got != TraeCNAuthHost {
		t.Fatalf("legacy exchange host = %q, want %q", got, TraeCNAuthHost)
	}
	custom := "http://127.0.0.1:18080"
	if got := traeCNExchangeHost(custom); got != custom {
		t.Fatalf("custom exchange host = %q, want %q", got, custom)
	}
}

func TestNormalizeTraeTokenResponseRejectsMissingToken(t *testing.T) {
	t.Parallel()
	_, err := normalizeTraeTokenResponse([]byte(`{"data":{"token":null,"message":"invalid refresh token","code":401}}`))
	if err == nil || strings.Contains(err.Error(), "<nil>") || !strings.Contains(err.Error(), "invalid refresh token") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExchangeTraeCNRefreshToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != TraeCNExchangePath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["ClientID"] != TraeCNOAuthClientID || body["ClientSecret"] != "-" || body["RefreshToken"] != "RT" {
			t.Errorf("unexpected request body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"token":"AT","refreshToken":"ROTATED","expiredAt":4102444800000,"userId":"uid"}}`))
	}))
	defer server.Close()

	token, err := ExchangeTraeCNRefreshToken(context.Background(), "RT", server.URL+"/", "")
	if err != nil {
		t.Fatalf("ExchangeTraeCNRefreshToken() error = %v", err)
	}
	if token.AccessToken != "AT" || token.RefreshToken != "ROTATED" || token.UserID != "uid" {
		t.Fatalf("unexpected token: %+v", token)
	}
	if token.ExpiresAt.Unix() != 4102444800 {
		t.Fatalf("expires_at = %s", token.ExpiresAt)
	}
}

func TestEnsureTraeCNAccountATOnlyExpiryHandling(t *testing.T) {
	t.Parallel()
	store := &Store{}
	for _, account := range []*Account{
		{UpstreamType: UpstreamTraeCN, AccessToken: "AT"},
		{UpstreamType: UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Minute)},
	} {
		if err := store.EnsureTraeCNAccount(context.Background(), account); err != nil {
			t.Fatalf("usable AT-only account rejected: %v", err)
		}
	}
	expired := &Account{UpstreamType: UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := store.EnsureTraeCNAccount(context.Background(), expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired AT-only account error = %v", err)
	}
}

func TestTraeCNRequestHeadersAreStablePerAccount(t *testing.T) {
	t.Parallel()
	account := &Account{DBID: 123, UpstreamType: UpstreamTraeCN, RefreshToken: "RT", TraeCNUserID: "uid"}
	first := TraeCNRequestHeaders(account, "AT", "request-1")
	account.RefreshToken = "ROTATED"
	second := TraeCNRequestHeaders(account, "AT", "request-2")
	// 抓包实测 agent 接口只用 x-ide-token 鉴权，不发 Authorization/x-cloudide-token。
	if first.Get("x-ide-token") != "AT" {
		t.Fatalf("missing client auth header x-ide-token: %#v", first)
	}
	if first.Get("Authorization") != "" || first.Get("X-Cloudide-Token") != "" {
		t.Fatalf("legacy auth headers must not be sent by default: %#v", first)
	}
	if first.Get("x-device-id") == "" || first.Get("x-device-id") != second.Get("x-device-id") {
		t.Fatalf("device id is not stable: %q vs %q", first.Get("x-device-id"), second.Get("x-device-id"))
	}
	if len(first.Get("x-device-id")) != 19 || strings.Trim(first.Get("x-device-id"), "0123456789") != "" {
		t.Fatalf("device id is not a 19-digit value: %q", first.Get("x-device-id"))
	}
	if first.Get("x-machine-id") == "" || first.Get("x-machine-id") == first.Get("x-device-id") {
		t.Fatalf("machine/device identity must be populated and distinct: machine=%q device=%q", first.Get("x-machine-id"), first.Get("x-device-id"))
	}
	if first.Get("x-ide-version") == "" || first.Get("x-ide-version-code") == "" || first.Get("x-os-version") == "" {
		t.Fatalf("desktop compatibility headers are incomplete: %#v", first)
	}
	if first.Get("User-Agent") != TraeCNDefaultUserAgent {
		t.Fatalf("User-Agent = %q, want Trae-compatible default", first.Get("User-Agent"))
	}
	if !strings.HasPrefix(first.Get("X-Request-ID"), "req_") || first.Get("X-Trae-Request-ID") != "request-1" {
		t.Fatalf("unexpected request id headers: %#v", first)
	}
}

func TestTraeCNDeviceIDMatchesDesktopHash(t *testing.T) {
	t.Parallel()
	machineID := "20ffdf2184b9e6cec15ab95f4445e057cca18535629e6bf1be6cb45af4c37718"
	if got := traeCNDeviceIDForMachineID(machineID); got != "0000000000249215914" {
		t.Fatalf("device id = %q, want desktop-compatible hash", got)
	}
}

func TestRefreshTraeCNAccountDBFirstPublication(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"new-at","refreshToken":"new-rt","expiresIn":3600,"userId":"uid-new"}`))
	}))
	defer server.Close()

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "traecn-db-first.db"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertAccountWithUpstream(context.Background(), "traecn", "trae", UpstreamTraeCN, map[string]any{
		"upstream_type": UpstreamTraeCN,
		"access_token":  "old-at",
		"refresh_token": "old-rt",
		"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"traecn_host":   server.URL,
	}, "")
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	account := store.FindByID(id)
	if account == nil {
		_ = db.Close()
		t.Fatal("Trae CN runtime account not loaded")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	err = store.RefreshTraeCNAccountByID(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "持久化 Trae CN credentials") {
		t.Fatalf("refresh error = %v, want persistence failure", err)
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.AccessToken != "old-at" || account.RefreshToken != "old-rt" || account.TraeCNUserID != "" {
		t.Fatalf("unpersisted credentials leaked into memory: at=%q rt=%q uid=%q", account.AccessToken, account.RefreshToken, account.TraeCNUserID)
	}
}

func TestRefreshTraeCNAccountRejectsDirectWithEmptyProxyPool(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"token":"new-at"}`))
	}))
	defer server.Close()
	store := &Store{proxyPoolEnabled: true}
	account := &Account{DBID: 9, UpstreamType: UpstreamTraeCN, RefreshToken: "rt", TraeCNHost: server.URL}

	err := store.refreshTraeCNAccount(context.Background(), account, true)
	if err == nil || !strings.Contains(err.Error(), "拒绝直连刷新") {
		t.Fatalf("refresh error = %v, want fail-closed proxy error", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("ExchangeToken calls = %d, want 0", calls.Load())
	}
}

func TestRefreshTraeCNAccountKeepsConcurrentCooldown(t *testing.T) {
	t.Parallel()
	store := &Store{maxConcurrency: 1}
	account := &Account{
		DBID: 10, UpstreamType: UpstreamTraeCN, AccessToken: "old-at", RefreshToken: "old-rt",
		Status: StatusReady, HealthTier: HealthTierHealthy,
	}
	cooldownUntil := time.Now().Add(5 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		account.mu.Lock()
		account.Status = StatusCooldown
		account.CooldownUtil = cooldownUntil
		account.CooldownReason = "rate_limited_model"
		account.mu.Unlock()
		_, _ = w.Write([]byte(`{"token":"new-at","refreshToken":"new-rt","expiresIn":3600}`))
	}))
	defer server.Close()
	account.TraeCNHost = server.URL

	if err := store.refreshTraeCNAccount(context.Background(), account, true); err != nil {
		t.Fatal(err)
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.AccessToken != "new-at" || account.RefreshToken != "new-rt" {
		t.Fatalf("credentials = %q/%q", account.AccessToken, account.RefreshToken)
	}
	if account.Status != StatusCooldown || account.CooldownReason != "rate_limited_model" || !account.CooldownUtil.Equal(cooldownUntil) {
		t.Fatalf("concurrent cooldown was cleared: status=%v reason=%q until=%v", account.Status, account.CooldownReason, account.CooldownUtil)
	}
}

func TestTraeCNRefreshPersistsRotatedCredentialsAfterCallerCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"cancel-safe-at","refreshToken":"cancel-safe-rt","expiresIn":3600,"userId":"cancel-safe-user"}`))
	}))
	defer server.Close()

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "traecn-cancel-safe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "cancel-safe", "trae", UpstreamTraeCN, map[string]any{
		"upstream_type": UpstreamTraeCN,
		"access_token":  "expired-at",
		"refresh_token": "cancel-safe-old-rt",
		"expires_at":    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"traecn_host":   server.URL,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}

	requestCtx, cancel := context.WithCancel(context.Background())
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- store.RefreshTraeCNAccountByID(requestCtx, id) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Trae CN refresh did not reach ExchangeToken")
	}
	cancel()
	close(release)

	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatalf("refresh after caller cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not finish after provider response")
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("access_token") != "cancel-safe-at" || row.GetCredential("refresh_token") != "cancel-safe-rt" {
		t.Fatalf("rotated credentials were not persisted after cancellation: at=%q rt=%q", row.GetCredential("access_token"), row.GetCredential("refresh_token"))
	}
}

func TestConcurrentForcedTraeCNRefreshCoalesces(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"token":"new-at","refreshToken":"new-rt","expiresIn":3600}`))
	}))
	defer server.Close()
	store := &Store{maxConcurrency: 1}
	account := &Account{DBID: 11, UpstreamType: UpstreamTraeCN, AccessToken: "old-at", RefreshToken: "old-rt", TraeCNHost: server.URL}

	// Hold the refresh mutex so both callers observe the same old credential
	// before either can exchange it.
	account.traeRefreshMu.Lock()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	started := make(chan struct{}, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			errs <- store.refreshTraeCNAccount(context.Background(), account, true)
		}()
	}
	<-started
	<-started
	time.Sleep(20 * time.Millisecond)
	account.traeRefreshMu.Unlock()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent refresh error = %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("ExchangeToken calls = %d, want 1", calls.Load())
	}
}

func TestTraeCNRefreshCASDoesNotOverwriteAdministratorReplacement(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"stale-at","refreshToken":"stale-rt","expiresIn":3600,"userId":"stale-user"}`))
	}))
	defer server.Close()

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "traecn-refresh-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "trae-cas", "trae", UpstreamTraeCN, map[string]any{
		"upstream_type": UpstreamTraeCN,
		"access_token":  "old-at",
		"refresh_token": "old-rt",
		"expires_at":    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"traecn_host":   server.URL,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- store.RefreshTraeCNAccountByID(ctx, id) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Trae CN refresh did not reach ExchangeToken")
	}

	adminExpiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := db.UpdateCredentials(ctx, id, map[string]any{
		"upstream_type":  UpstreamTraeCN,
		"access_token":   "admin-at",
		"refresh_token":  "admin-rt",
		"expires_at":     adminExpiry,
		"traecn_user_id": "admin-user",
		"account_id":     "admin-user",
		"email":          "admin@example.invalid",
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatalf("refresh after administrator replacement: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not finish after provider response")
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("access_token") != "admin-at" || row.GetCredential("refresh_token") != "admin-rt" || row.CredentialGeneration != 2 {
		t.Fatalf("administrator credential was overwritten: generation=%d at=%q rt=%q", row.CredentialGeneration, row.GetCredential("access_token"), row.GetCredential("refresh_token"))
	}
	account := store.FindByID(id)
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.AccessToken != "admin-at" || account.RefreshToken != "admin-rt" || account.AccountID != "admin-user" || account.CredentialGeneration != 2 {
		t.Fatalf("runtime credential did not reload administrator replacement: generation=%d at=%q rt=%q account=%q", account.CredentialGeneration, account.AccessToken, account.RefreshToken, account.AccountID)
	}
}
