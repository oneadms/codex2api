package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// A key with the default (auto) upstream channel must not silently fall back
// to a Trae CN account when a Codex model has no Codex account available.  The
// previous resolver treated every Trae account with an empty Models list as a
// generic relay, so this request reached the Trae converter and failed with a
// misleading 400 (for example, additional_tools is not representable there).
func TestDefaultChannelCodexRequestDoesNotFallbackToTraeCN(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"unexpected\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	defer store.Stop()
	store.AddAccount(&auth.Account{
		DBID:         91010,
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "trae-at",
		RefreshToken: "trae-rt",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   upstream.URL,
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	// additional_tools is valid Responses input but intentionally unsupported by
	// the Trae converter; seeing that converter's 400 would prove misrouting.
	body := []byte(`{"model":"gpt-5.4","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	// Mark the fixture as a real Codex client.  This is the same required
	// engine-fingerprint signal used by production affinity routing.
	ctx.Request.Header.Set("X-Codex-Trace", "routing-regression")
	// No UpstreamChannel field means the default/auto channel.
	ctx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91011})

	handler.Responses(ctx)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if payload.Error.Code != ErrorCodeNoAvailableAccount || payload.Error.Type != ErrorTypeServerError {
		t.Fatalf("error = %#v, want no_available_account/server_error; body=%s", payload.Error, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "Trae CN request conversion failed") || strings.Contains(recorder.Body.String(), "additional_tools") {
		t.Fatalf("Trae conversion error leaked for a Codex request: %s", recorder.Body.String())
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("Trae upstream calls = %d, want 0", got)
	}
}

// Keep the regression close to the account resolver as well as the endpoint:
// an auto-channel key's normal Responses filter must reject Trae for a model
// belonging to the Codex catalog, while the explicit Trae channel remains a
// separate opt-in path.
func TestResponsesResolverSeparatesCodexAndTraeCNChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	trae := &auth.Account{
		DBID:         91012,
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "trae-at",
		RefreshToken: "trae-rt",
	}
	base := accountFilterForResponsesModel("gpt-5.4", true)

	autoCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	autoCtx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91013})
	autoFilter := (&Handler{}).applyUpstreamChannelFilter(autoCtx, "gpt-5.4", base)
	if autoFilter(trae) {
		t.Fatal("default/auto Responses routing admitted Trae CN for a Codex model")
	}
	if (&Handler{}).applyUpstreamChannelFilter(autoCtx, "auto", accountFilterForResponsesModel("auto", false))(trae) {
		t.Fatal("default/auto Responses routing admitted Trae CN for the auto model")
	}

	codexCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	codexCtx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91014, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelCodex}})
	codexFilter := (&Handler{}).applyUpstreamChannelFilter(codexCtx, "gpt-5.4", base)
	if codexFilter(trae) {
		t.Fatal("explicit Codex channel admitted Trae CN")
	}

	traeCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	traeCtx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91015, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
	traeFilter := (&Handler{}).applyUpstreamChannelFilter(traeCtx, "deepseek-v3", accountFilterForResponsesModel("deepseek-v3", false))
	if !traeFilter(trae) {
		t.Fatal("explicit Trae CN channel rejected a supported Trae model")
	}
}

func TestScopedModelsHideTraeCNFromDefaultChannel(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	store.AddAccount(&auth.Account{
		DBID:         91016,
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "trae-at",
		RefreshToken: "trae-rt",
		Models:       []string{"deepseek-v3"},
	})
	handler := NewHandler(store, nil, nil, nil)

	auto := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 91017})
	if _, _, ok := scopedModelByID(auto, "deepseek-v3"); ok {
		t.Fatalf("default-channel model catalog advertised a TRAECN-only model: %+v", auto)
	}

	trae := listScopedModelsForTest(t, handler, &database.APIKeyRow{
		ID:     91018,
		Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN},
	})
	if owner, _, ok := scopedModelByID(trae, "deepseek-v3"); !ok || owner != "trae" {
		t.Fatalf("explicit TRAECN catalog = %+v, want deepseek-v3 owned by trae", trae)
	}
}
