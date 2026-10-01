package proxy

import (
	"context"
	"fmt"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTraeCNUsageUsesMappedProviderPricing(t *testing.T) {
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous); database.SetModelPricingOverrides(nil) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"gpt-5.4": "doubao-1-6"}})
	database.SetModelPricingOverrides(map[string]database.ModelPricingOverride{
		"doubao_1_6": {Input: 0.3, Output: 0.6, CachedInput: 0.1, Source: database.ModelPricingSourceCustom},
		"gpt-5.4":    {Input: 10, Output: 20, Source: database.ModelPricingSourceCustom},
	})
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNUpstreamModelCatalog: []string{"Doubao_1_6"}}
	model := traeCNUsageEffectiveModel(account, "gpt-5.4")
	if model != "Doubao_1_6" {
		t.Fatalf("billing model = %q", model)
	}
	log := database.SnapshotUsageLogBilling(&database.UsageLogInput{Model: "gpt-5.4", EffectiveModel: model, InputTokens: 1000000, OutputTokens: 1000000, CachedTokens: 500000})
	if got := database.UsageLogBilledCost(log); got < 0.799999 || got > 0.800001 {
		t.Fatalf("billed cost = %v, want 0.8", got)
	}
	if got := database.UsageLogUserBilledCost(log); got < 0.799999 || got > 0.800001 {
		t.Fatalf("user cost = %v, want 0.8", got)
	}
}

func TestTraeCNMappedUsageLogsUseProviderCost(t *testing.T) {
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous); database.SetModelPricingOverrides(nil) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"gpt-5.4": "Doubao_1_6"}})
	database.SetModelPricingOverrides(map[string]database.ModelPricingOverride{
		"doubao_1_6": {Input: 0.3, Output: 0.6, CachedInput: 0.1, Source: database.ModelPricingSourceCustom},
	})
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			path, body string
			invoke     func(*Handler, *gin.Context)
		}{
			{"/v1/responses", `{"input":"hello"}`, (*Handler).Responses},
			{"/v1/chat/completions", `{"messages":[{"role":"user","content":"hello"}]}`, (*Handler).ChatCompletions},
			{"/v1/messages", `{"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, (*Handler).Messages},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.path, stream), func(t *testing.T) {
				db, err := database.New("sqlite", filepath.Join(t.TempDir(), "traecn-pricing.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				db.SetUsageLogConfig(database.UsageLogModeFull, 100, 60)
				handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: output\ndata: {\"text\":\"hello\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":1000000,\"completion_tokens\":1000000,\"cached_tokens\":500000}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
				})
				handler.db = db
				body := strings.TrimSuffix(tc.body, "}") + fmt.Sprintf(`,"model":"gpt-5.4","service_tier":"priority","stream":%t}`, stream)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91088, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
				tc.invoke(handler, c)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				db.FlushUsageLogs()
				logs, err := db.ListUsageLogsByTimeRange(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
				if err != nil || len(logs) != 1 {
					t.Fatalf("logs=%+v err=%v", logs, err)
				}
				entry := logs[0]
				if entry.EffectiveModel != "Doubao_1_6" || entry.Channel != database.UpstreamChannelTraeCN || math.Abs(entry.TotalCost-0.8) > 1e-8 || math.Abs(entry.AccountBilled-0.8) > 1e-8 || math.Abs(entry.UserBilled-0.8) > 1e-8 {
					t.Fatalf("incorrect mapped billing: %+v", entry)
				}
			})
		}
	}
}
