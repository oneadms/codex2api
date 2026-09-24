package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// A TRAECN-only key can use an official-looking alias while the pool also has
// real Codex accounts. Their learned capabilities must not erase Trae's bridge.
func TestTraeCNManifestAliasKeepsDirectModeWithStoredCodexCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestModelRegistryDB(t)
	id, err := db.InsertAccount(t.Context(), "official", "test-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: id, CredentialGeneration: row.CredentialGeneration, AccessToken: "test-access", PlanType: "plus"})
	store.AddAccount(&auth.Account{DBID: id + 1, UpstreamType: auth.UpstreamTraeCN, AccessToken: "test-trae", TraeCNUpstreamModelCatalog: []string{"kimi-k3"}})
	err = db.SaveModelCapabilities(t.Context(), database.ModelCapabilitySnapshot{AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: time.Now().UnixNano(), Models: map[string]map[string]json.RawMessage{"gpt-6-astra": {
		"tool_mode":             json.RawMessage("\"code_mode_only\""),
		"apply_patch_tool_type": json.RawMessage("\"freeform\""),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, db, nil, nil)
	previous := auth.ConfiguredTraeCNSettings()
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"gpt-6-astra": "kimi-k3"}})
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	key := &database.APIKeyRow{ID: 987654, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.140.0", nil)
	c.Set(contextAPIKeyRow, key)
	h.CodexModelsManifestHandler(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("manifest HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	model := gjson.GetBytes(recorder.Body.Bytes(), `models.#(slug=="gpt-6-astra")`)
	if model.Get("tool_mode").String() != "direct" || model.Get("apply_patch_tool_type").String() != "freeform" {
		t.Fatalf("TRAE bridge capabilities erased by an unrelated Codex snapshot: %s", recorder.Body.String())
	}
}

func TestTraeCNManifestMappedAliasInheritsBridgeCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1234, UpstreamType: auth.UpstreamTraeCN, AccessToken: "test-trae", TraeCNUpstreamModelCatalog: []string{"kimi-k3"}})
	previous := auth.ConfiguredTraeCNSettings()
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"gpt-6-astra": "kimi-k3"}})
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	h := NewHandler(store, nil, nil, nil)
	key := &database.APIKeyRow{ID: 1235, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}}
	for _, model := range h.scopedModels(t.Context(), key) {
		if model.ID == "gpt-6-astra" && model.OwnedBy != "codex2api" {
			t.Fatal("model list alias ownership changed")
		}
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.140.0", nil)
	c.Set(contextAPIKeyRow, key)
	h.CodexModelsManifestHandler(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("manifest HTTP %d", recorder.Code)
	}
	for _, slug := range []string{"gpt-6-astra", "kimi-k3"} {
		model := gjson.GetBytes(recorder.Body.Bytes(), `models.#(slug=="`+slug+`")`)
		var efforts []string
		for _, level := range model.Get("supported_reasoning_levels").Array() {
			efforts = append(efforts, level.Get("effort").String())
		}
		if model.Get("tool_mode").String() != "direct" || model.Get("apply_patch_tool_type").String() != "freeform" || !slices.Contains(efforts, "max") || model.Get("prefer_websockets").Bool() {
			t.Fatalf("%s lost TRAECN capabilities: %s", slug, model.Raw)
		}
	}
}
