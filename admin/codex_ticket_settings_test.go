package admin

import (
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func codexTicketTestRouter(t *testing.T) (*gin.Engine, func(string) *httptest.ResponseRecorder, func() string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredCodexTicketSettings()
	t.Cleanup(func() { auth.SetConfiguredCodexTicketSettings(previous) })
	auth.SetConfiguredCodexTicketSettings(auth.DefaultCodexTicketSettings())
	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/settings/codex-ticket", handler.GetCodexTicketSettings)
	router.PUT("/settings/codex-ticket", handler.UpdateCodexTicketSettings)
	put := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/settings/codex-ticket", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		return recorder
	}
	get := func() string {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings/codex-ticket", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("get status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}
	return router, put, get
}

// 打票配置独立读写 /settings/codex-ticket，改动即时保存，通用设置保存不能覆盖它。
func TestCodexTicketSettingsRoundTrip(t *testing.T) {
	db := newTestAdminDB(t)
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredCodexTicketSettings()
	t.Cleanup(func() { auth.SetConfiguredCodexTicketSettings(previous) })
	auth.SetConfiguredCodexTicketSettings(auth.DefaultCodexTicketSettings())

	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/settings/codex-ticket", handler.GetCodexTicketSettings)
	router.PUT("/settings/codex-ticket", handler.UpdateCodexTicketSettings)
	put := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/settings/codex-ticket", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		return recorder
	}

	// 默认关闭，且门控未生效。
	if auth.CodexTicketGateEnabled() {
		t.Fatal("harvesting must default to off")
	}
	if rec := put(`{"enabled":true,"harvest_proxy_url":"socks5h://user:secret@127.0.0.1:1080","models":["gpt-5.5"]}`); rec.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !auth.CodexTicketGateEnabled() {
		t.Fatal("gate must be active after enabling with proxy and models")
	}
	// 代理密码绝不能回显原文。
	if body := recBody(router, "/settings/codex-ticket"); strings.Contains(body, "secret") {
		t.Fatalf("proxy password leaked: %s", body)
	} else if !strings.Contains(body, "***") {
		t.Fatalf("masked proxy missing: %s", body)
	}

	raw, err := db.LoadCodexTicketConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.ParseCodexTicketSettings(raw)
	if err != nil || !parsed.Enabled || len(parsed.Models) != 1 {
		t.Fatalf("persisted=%s err=%v", raw, err)
	}
	// 通用设置保存不能覆盖独立配置。
	settings, err := db.GetSystemSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSystemSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	reloaded, err := db.LoadCodexTicketConfig(t.Context())
	if err != nil || reloaded != raw {
		t.Fatalf("config overwritten: %s err=%v", reloaded, err)
	}
}

// 掩码回显不能被当成新密码存回去。
func TestCodexTicketSettingsKeepsPasswordOnMaskedResubmit(t *testing.T) {
	_, put, get := codexTicketTestRouter(t)
	if rec := put(`{"enabled":true,"harvest_proxy_url":"socks5h://user:secret@127.0.0.1:1080","models":["gpt-5.5"]}`); rec.Code != http.StatusOK {
		t.Fatalf("seed: %s", rec.Body.String())
	}
	masked := auth.MaskCodexTicketProxyURL(auth.ConfiguredCodexTicketSettings().HarvestProxyURL)
	if masked == "" {
		t.Fatal("expected a masked proxy")
	}
	// 把掩码原样提交回来，并同时改一个无关字段。
	if rec := put(`{"harvest_proxy_url":"` + masked + `","max_probes_per_round":3}`); rec.Code != http.StatusOK {
		t.Fatalf("masked resubmit: %s", rec.Body.String())
	}
	if got := auth.ConfiguredCodexTicketSettings().HarvestProxyURL; !strings.Contains(got, "secret") {
		t.Fatalf("password must be preserved, got %q", got)
	}
	if got := auth.ConfiguredCodexTicketSettings().MaxProbesPerRound; got != 3 {
		t.Fatalf("unrelated field not updated: %d", got)
	}
	if body := get(); strings.Contains(body, "secret") {
		t.Fatalf("password leaked in GET: %s", body)
	}
}

// 启用打票必须同时有代理与门控模型，否则门控空转。
func TestCodexTicketSettingsRejectsIncompleteEnable(t *testing.T) {
	_, put, _ := codexTicketTestRouter(t)
	for _, bad := range []string{
		`{}`,
		`{"enabled":true}`,
		`{"enabled":true,"models":["gpt-5.5"]}`,
		`{"enabled":true,"harvest_proxy_url":"socks5h://127.0.0.1:1080"}`,
		`{"harvest_proxy_url":"ftp://127.0.0.1:1080"}`,
		`{"harvest_proxy_url":"socks5h://127.0.0.1:1080/path"}`,
		`{"target_length":99999}`,
	} {
		if rec := put(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid request=%s status=%d body=%s", bad, rec.Code, rec.Body.String())
		}
	}
	if auth.CodexTicketGateEnabled() {
		t.Fatal("rejected updates must not enable the gate")
	}
}

func TestCodexTicketReadyAccountCountsOnlyEligibleCodexAccounts(t *testing.T) {
	raw := make([]byte, auth.CodexTicketEnvelopeHeaderBytes+auth.CodexTicketBlockBytes*auth.CodexTicketPersonalBlocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Unix()))
	ticket := &auth.CodexTicket{
		Model: "gpt-6-astra", State: base64.URLEncoding.EncodeToString(raw), Length: 292, Cookie: "ticket=ready",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	store := &auth.Store{}
	store.SetAccountsForTest([]*auth.Account{
		nil,
		{DBID: 1, UpstreamType: "grok", AccessToken: "grok-at"},
		{DBID: 2, UpstreamType: "openai_responses", APIKey: "relay-key"},
		{DBID: 3, AccessToken: "disabled-at", Disabled: 1},
		{DBID: 4, CodexAuthMode: auth.CodexAuthModeAgentIdentity},
		{DBID: 5, AccessToken: "codex-at", CodexTickets: map[string]*auth.CodexTicket{"gpt-6-astra": ticket}},
		{DBID: 6, RefreshToken: "codex-rt"},
	})
	handler := &Handler{store: store}
	ready, total := handler.codexTicketReadyAccountCounts([]string{"gpt-6-astra"})
	if ready != 1 || total != 2 {
		t.Fatalf("ready/total = %d/%d, want 1/2", ready, total)
	}
}

func recBody(router *gin.Engine, path string) string {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Body.String()
}
