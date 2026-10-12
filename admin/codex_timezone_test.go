package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func newCodexTimezoneTestHandler(t *testing.T) *Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousURL, previousResin := codexTimezoneLookupURL, proxy.GetResinConfig()
	codexTimezoneLookupURL = "http://ip-lookup.test/"
	proxy.SetResinConfig(nil)
	t.Cleanup(func() {
		codexTimezoneLookupURL = previousURL
		proxy.SetResinConfig(previousResin)
	})
	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{LazyMode: true, MaxConcurrency: 4})
	t.Cleanup(func() {
		db.DrainBackgroundTasks(time.Second)
		store.Stop()
	})
	return &Handler{db: db, store: store, probeUsage: func(context.Context, *auth.Account) error { return nil }}
}

func TestCodexTimezoneBindUsesAccountProxyAndPersists(t *testing.T) {
	h := newCodexTimezoneTestHandler(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Host != "ip-lookup.test" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Secret") != "" {
			t.Errorf("unexpected lookup request: %s, %v", r.URL, r.Header)
		}
		_, _ = w.Write([]byte(`{"success":true,"ip":"203.0.113.9","timezone":{"id":"America/New_York"}}`))
	}))
	defer server.Close()
	id, err := h.db.InsertAccountWithCredentials(context.Background(), "test", map[string]interface{}{
		"refresh_token": "private-rt", "access_token": "private-at", "timezone": "Asia/Shanghai",
		"custom_headers": map[string]string{"X-Secret": "private-header"},
	}, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	h.store.AddAccount(&auth.Account{DBID: id, ProxyURL: server.URL, Timezone: "Asia/Shanghai"})
	router := gin.New()
	router.POST("/accounts/:id/timezone/bind-from-ip", h.BindCodexTimezoneFromIP)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/timezone/bind-from-ip", id), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("bind status=%d: %s", recorder.Code, recorder.Body.String())
	}
	var result codexExitTimezone
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	row, err := h.db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if result.IP != "203.0.113.9" || result.Timezone != "America/New_York" || calls.Load() != 1 {
		t.Fatalf("unexpected lookup result: %+v, calls=%d", result, calls.Load())
	}
	if row.GetCredential("timezone") != result.Timezone || h.store.FindByID(id).EffectiveCodexTimezone() != result.Timezone || row.GetCredential("refresh_token") != "private-rt" {
		t.Fatal("binding must persist, update runtime, and preserve credentials")
	}
}

func TestCodexTimezoneBindRejectsInvalidAccounts(t *testing.T) {
	h := newCodexTimezoneTestHandler(t)
	router := gin.New()
	router.POST("/accounts/:id/timezone/bind-from-ip", h.BindCodexTimezoneFromIP)
	for _, upstream := range []string{auth.UpstreamOpenAIResponses, auth.UpstreamGrok, auth.UpstreamClaude, auth.UpstreamAntigravity, auth.UpstreamTraeCN} {
		t.Run(upstream, func(t *testing.T) {
			id, err := h.db.InsertAccountWithCredentials(context.Background(), upstream, map[string]interface{}{
				"upstream_type": upstream, "api_key": "secret", "refresh_token": "rt",
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/timezone/bind-from-ip", id), nil))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("unsupported account status=%d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	for _, tc := range []struct {
		id     string
		status int
	}{{"invalid", http.StatusBadRequest}, {"0", http.StatusBadRequest}, {"999999", http.StatusNotFound}} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/"+tc.id+"/timezone/bind-from-ip", nil))
		if recorder.Code != tc.status {
			t.Errorf("id %s status=%d, want %d", tc.id, recorder.Code, tc.status)
		}
	}
}

func TestCodexTimezoneLookupFailurePreservesBinding(t *testing.T) {
	h := newCodexTimezoneTestHandler(t)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http failure", 429, `{}`},
		{"invalid json", 200, `not-json`},
		{"lookup rejected", 200, `{"success":false,"ip":"203.0.113.8","timezone":{"id":"UTC"}}`},
		{"missing ip", 200, `{"success":true,"timezone":{"id":"UTC"}}`},
		{"invalid timezone", 200, `{"success":true,"ip":"203.0.113.8","timezone":{"id":"Mars/Olympus"}}`},
		{"local timezone", 200, `{"success":true,"ip":"203.0.113.8","timezone":{"id":"Local"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			id, err := h.db.InsertAccountWithCredentials(context.Background(), tc.name, map[string]interface{}{"timezone": "Asia/Tokyo"}, server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.bindCodexTimezoneFromIP(context.Background(), id, false); err == nil {
				t.Fatal("invalid lookup must fail")
			}
			row, err := h.db.GetAccountByID(context.Background(), id)
			if err != nil || row.GetCredential("timezone") != "Asia/Tokyo" {
				t.Fatalf("original binding changed: row=%+v, error=%v", row, err)
			}
			if h.store.FindByID(id) != nil {
				t.Fatal("lookup must not add an unloaded account to the runtime pool")
			}
		})
	}
}

func TestCodexTimezoneFirstAddUsesBoundGroupProxyOnce(t *testing.T) {
	for _, tc := range []struct {
		name, path, credential string
		at                     bool
	}{
		{"rt", "/accounts", `"refresh_token":"rt-new","skip_refresh":true`, false},
		{"rt stream", "/accounts?stream=true", `"refresh_token":"rt-new","skip_refresh":true`, false},
		{"st", "/accounts", `"session_token":"st-new","skip_refresh":true`, false},
		{"at", "/accounts/at", `"access_token":"opaque-token"`, true},
		{"at stream", "/accounts/at?stream=true", `"access_token":"opaque-token"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCodexTimezoneTestHandler(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(`{"success":true,"ip":"2001:db8::9","timezone":{"id":"Europe/London"}}`))
			}))
			defer server.Close()
			groupID, err := h.db.CreateAccountGroup(context.Background(), "group", "", "", 0, 0, sql.NullInt64{})
			if err != nil {
				t.Fatal(err)
			}
			h.store.SetGroupProxyURLs(groupID, []string{server.URL})
			body := fmt.Sprintf(`{%s,"group_ids":[%d]}`, tc.credential, groupID)
			for attempt := 0; attempt < 2; attempt++ {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				if tc.at {
					h.AddATAccount(c)
				} else {
					h.AddAccount(c)
				}
				if recorder.Code != http.StatusOK {
					t.Fatalf("add status=%d: %s", recorder.Code, recorder.Body.String())
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					rows, err := h.db.ListActive(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if len(rows) == 1 && rows[0].GetCredential("timezone") == "Europe/London" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("new account was not automatically bound through its group proxy")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("re-adding an existing account repeated lookup: calls=%d", calls.Load())
			}
		})
	}
}

func TestCodexTimezoneLookupUsesResinIdentity(t *testing.T) {
	newCodexTimezoneTestHandler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/http/ip-lookup.test/" || r.Header.Get("X-Resin-Account") != "42" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Resin lookup: %s, %v", r.URL, r.Header)
		}
		_, _ = w.Write([]byte(`{"success":true,"ip":"203.0.113.42","timezone":{"id":"Asia/Tokyo"}}`))
	}))
	defer server.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex"})
	result, err := queryCodexExitTimezone(context.Background(), &auth.Account{DBID: 42, AccessToken: "secret"}, "http://127.0.0.1:1")
	if err != nil || result.Timezone != "Asia/Tokyo" {
		t.Fatalf("Resin lookup failed: %+v, %v", result, err)
	}
}

func TestCodexTimezoneUnavailableProxyDoesNotUseDirectEgress(t *testing.T) {
	h := newCodexTimezoneTestHandler(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"success":true,"ip":"203.0.113.9","timezone":{"id":"UTC"}}`))
	}))
	defer server.Close()
	codexTimezoneLookupURL = server.URL
	id, err := h.db.InsertAccount(context.Background(), "direct", "rt", "")
	if err != nil {
		t.Fatal(err)
	}
	h.store.SetProxyPoolEnabled(true)
	if _, err := h.bindCodexTimezoneFromIP(context.Background(), id, false); err == nil || calls.Load() != 0 {
		t.Fatalf("unavailable pool must block direct lookup: err=%v, calls=%d", err, calls.Load())
	}
	h.store.SetProxyPoolEnabled(false)
	if result, err := h.bindCodexTimezoneFromIP(context.Background(), id, false); err != nil || result.Timezone != "UTC" {
		t.Fatalf("explicit direct egress should work: result=%+v, err=%v", result, err)
	}
	if _, err := queryCodexExitTimezone(context.Background(), &auth.Account{DBID: id}, "://invalid-proxy"); err == nil || calls.Load() != 1 {
		t.Fatalf("invalid proxy must not fall back to direct lookup: err=%v, calls=%d", err, calls.Load())
	}
}

func TestCodexTimezoneAutomaticBindingPreservesManualSelection(t *testing.T) {
	h := newCodexTimezoneTestHandler(t)
	var id int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h.db.UpdateCredentials(r.Context(), id, map[string]interface{}{"timezone": "Asia/Shanghai"}); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"success":true,"ip":"203.0.113.9","timezone":{"id":"Europe/London"}}`))
	}))
	defer server.Close()
	var err error
	id, err = h.db.InsertAccount(context.Background(), "manual", "rt", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.bindCodexTimezoneFromIP(context.Background(), id, true)
	if err != nil || result != nil {
		t.Fatalf("automatic binding should defer to manual selection: %+v, %v", result, err)
	}
	// 第二次调用应在查询前跳过，避免覆盖已经选好的时区。
	row, _ := h.db.GetAccountByID(context.Background(), id)
	if row.GetCredential("timezone") != "Asia/Shanghai" {
		t.Fatal("automatic binding overwrote manual selection")
	}
	if result, err := h.bindCodexTimezoneFromIP(context.Background(), id, true); err != nil || result != nil {
		t.Fatalf("bound account should be skipped: %+v, %v", result, err)
	}
}

func TestCodexTimezoneOAuthBindsAfterLeaseInheritance(t *testing.T) {
	h := newCodexTimezoneTestHandler(t)
	var inherited atomic.Bool
	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/codex/https/auth.openai.com/oauth/token":
			_, _ = w.Write([]byte(`{"access_token":"oauth-at","refresh_token":"oauth-rt","expires_in":3600}`))
		case "/api/v1/codex/actions/inherit-lease":
			inherited.Store(true)
			_, _ = w.Write([]byte(`{}`))
		case "/codex/http/ip-lookup.test/":
			if !inherited.Load() || r.Header.Get("X-Resin-Account") == "" {
				t.Error("timezone lookup must follow lease inheritance and carry the account identity")
			}
			lookups.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"ip":"203.0.113.10","timezone":{"id":"Asia/Tokyo"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex"})
	sessionID := "timezone-oauth"
	globalOAuthStore.set(sessionID, &oauthSession{
		State: "state", CodeVerifier: "verifier", RedirectURI: oauthDefaultRedirectURI, CreatedAt: time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/oauth/exchange-code", strings.NewReader(`{"session_id":"timezone-oauth","state":"state","code":"code"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ExchangeOAuthCode(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("exchange status=%d: %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		row, err := h.db.GetAccountByID(context.Background(), result.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.GetCredential("timezone") == "Asia/Tokyo" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OAuth account was not automatically bound")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lookups.Load() != 1 {
		t.Fatalf("OAuth account should query once, got %d", lookups.Load())
	}
}

func TestCodexTimezoneFileImportAutomaticallyBinds(t *testing.T) {
	h := newCodexTimezoneTestHandler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"ip":"203.0.113.20","timezone":{"id":"Europe/Berlin"}}`))
	}))
	defer server.Close()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/accounts/import", nil)
	h.importAccountsCommon(c, []importToken{{refreshToken: "rt-file"}}, importSettings{defaultProxyURL: server.URL})
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"type":"complete"`) {
		t.Fatalf("import status=%d: %s", recorder.Code, recorder.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err := h.db.ListActive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].GetCredential("timezone") == "Europe/Berlin" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("file import did not schedule an automatic timezone binding")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
