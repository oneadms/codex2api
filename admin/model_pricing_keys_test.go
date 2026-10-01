package admin

import (
	"encoding/json"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestModelPricingManagementKeysKeepsIndependentAlias(t *testing.T) {
	got := modelPricingManagementKeys([]string{
		"gpt-5.4",
		"codex-auto-review",
		"gpt-5.4-openai-compact",
	})
	want := []string{"gpt-5.4", "codex-auto-review"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modelPricingManagementKeys() = %v, want %v", got, want)
	}
}

func TestListModelPricingIncludesTraeCNAccountAndDefaultModels(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 987, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNUpstreamModelCatalog: []string{"private-trae-model", "Doubao_1_6"}})
	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ListModelPricing(c)
	var response struct {
		Models []modelPricingRow `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool)
	for _, row := range response.Models {
		if row.Channel == database.UpstreamChannelTraeCN {
			found[row.Model] = true
		}
	}
	for _, model := range []string{"private-trae-model", "doubao_1_6", "auto", "glm-5.3"} {
		if !found[model] {
			t.Fatalf("missing TRAECN pricing row %q", model)
		}
	}
}
