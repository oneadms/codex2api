package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// 真实抓包形状：两个积分池都在权益包里，available_endpoint=0 是 IDE/Code 侧，
// =1 是 Work 专属；usage_summary 是两个池的合计，不能当单池余额用。
const traeCNCreditsPacksFixture = `{
  "is_credits_billing": true,
  "is_dollar_usage_billing": false,
  "usage_summary": {"total_amount": 5250, "consumed_amount": 3250, "consumption_ratio": 0.6190476190476191},
  "user_entitlement_pack_list": [
    {"entitlement_base_info": {"entitlement_id": "general", "product_id": 208, "available_endpoint": 0,
      "quota": {"credits_limit": 2000}}, "usage": {"credits_amount": 2000}},
    {"entitlement_base_info": {"entitlement_id": "work", "product_id": 209, "available_endpoint": 1,
      "quota": {"credits_limit": 2000}}, "usage": {}},
    {"entitlement_base_info": {"entitlement_id": "free", "product_id": 0,
      "quota": {"enable_solo_agent": true, "enable_solo_coder": true}}, "usage": {}},
    {"entitlement_base_info": {"entitlement_id": "monthly", "product_id": 221, "available_endpoint": 0,
      "quota": {"credits_limit": 500}}, "usage": {"credits_amount": 500}},
    {"entitlement_base_info": {"entitlement_id": "checkin", "product_id": 208, "available_endpoint": 0,
      "quota": {"credits_limit": 150}}, "usage": {"credits_amount": 150}}
  ]
}`

// IDE 侧视角看不到 Work 专属包，只剩通用包。
const traeCNCreditsCodeOnlyFixture = `{
  "is_credits_billing": true,
  "usage_summary": {"total_amount": 3250, "consumed_amount": 3250},
  "user_entitlement_pack_list": [
    {"entitlement_base_info": {"entitlement_id": "general", "product_id": 208, "available_endpoint": 0,
      "quota": {"credits_limit": 2000}}, "usage": {"credits_amount": 2000}},
    {"entitlement_base_info": {"entitlement_id": "monthly", "product_id": 221, "available_endpoint": 0,
      "quota": {"credits_limit": 500}}, "usage": {"credits_amount": 500}},
    {"entitlement_base_info": {"entitlement_id": "checkin", "product_id": 208, "available_endpoint": 0,
      "quota": {"credits_limit": 150}}, "usage": {"credits_amount": 150}}
  ]
}`

func TestParseTraeCNCreditsSplitsPoolsByEndpoint(t *testing.T) {
	now := time.Now()
	snapshot, err := parseTraeCNCredits([]byte(traeCNCreditsPacksFixture), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pools) != 2 {
		t.Fatalf("pools = %+v, want Code and Work", snapshot.Pools)
	}
	code, ok := snapshot.Pool(TraeCNCreditsPoolCode)
	if !ok || code.Total != 2650 || code.Used != 2650 || code.Remaining != 0 || math.Abs(code.UsedPercent-100) > 1e-9 || !code.UpdatedAt.Equal(now) {
		t.Fatalf("code pool = %+v", code)
	}
	work, ok := snapshot.Pool(TraeCNCreditsPoolWork)
	if !ok || work.Total != 2000 || work.Used != 0 || work.Remaining != 2000 || work.UsedPercent != 0 {
		t.Fatalf("work pool = %+v", work)
	}
}

func TestParseTraeCNCreditPoolsIgnoreFeatureEntitlements(t *testing.T) {
	// enable_solo_* 这类没有 credits_limit 的功能权益不能算成积分包，
	// 否则总量会被夸大。
	snapshot, err := parseTraeCNCredits([]byte(traeCNCreditsPacksFixture), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, pool := range snapshot.Pools {
		total += pool.Total
	}
	if total != 4650 {
		t.Fatalf("pools total = %v, want 4650", total)
	}
}

func TestParseTraeCNCreditsDedupesRepeatedPacks(t *testing.T) {
	pack := `{"entitlement_base_info": {"entitlement_id": "general", "available_endpoint": 0, "quota": {"credits_limit": 100}}, "usage": {"credits_amount": 10}}`
	payload := `{"is_credits_billing": true, "user_entitlement_pack_list": [` + pack + `,` + pack + `]}`
	snapshot, err := parseTraeCNCredits([]byte(payload), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, _ := snapshot.Pool(TraeCNCreditsPoolCode)
	if code.Total != 100 || code.Used != 10 || code.Remaining != 90 {
		t.Fatalf("repeated pack double counted: %+v", code)
	}
}

func TestParseTraeCNCreditsRejectsMissingOrDifferentBilling(t *testing.T) {
	for _, payload := range []string{
		`<html>upstream error</html>`,
		`{}`,
		`{"is_credits_billing":false,"user_entitlement_pack_list":[{"entitlement_base_info":{"available_endpoint":0,"quota":{"credits_limit":10}},"usage":{"credits_amount":1}}]}`,
		`{"is_credits_billing":true,"is_dollar_usage_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"available_endpoint":0,"quota":{"credits_limit":10}},"usage":{"credits_amount":1}}]}`,
		`{"is_credits_billing":true,"user_entitlement_pack_list":[]}`,
		`{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"available_endpoint":0,"quota":{"credits_limit":0}},"usage":{"credits_amount":0}}]}`,
		`{"code":3004,"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"available_endpoint":0,"quota":{"credits_limit":10}},"usage":{"credits_amount":1}}]}`,
	} {
		if got, err := parseTraeCNCredits([]byte(payload), time.Now()); err == nil || len(got.Pools) != 0 {
			t.Fatalf("invalid response accepted: %s -> %+v, %v", payload, got, err)
		}
	}
}

func TestQueryTraeCNCreditsUsesWorkScopeAndRecordsBalance(t *testing.T) {
	var mu sync.Mutex
	sources := []int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != traeCNCreditsUsagePath {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT credits-test-token" || r.Header.Get("x-market-client-id") != auth.TraeCNMarketClientID || r.Header.Get("x-device-id") == "" || r.Header.Get("x-ide-token") != "" {
			t.Error("market identity missing or inference identity used")
		}
		raw, _ := io.ReadAll(r.Body)
		if !gjson.GetBytes(raw, "require_usage").Bool() {
			t.Errorf("request body = %s", raw)
		}
		reqSource := gjson.GetBytes(raw, "req_source").Int()
		mu.Lock()
		sources = append(sources, reqSource)
		mu.Unlock()
		// Work 视角能同时看到两个池。
		_, _ = io.WriteString(w, traeCNCreditsPacksFixture)
	}))
	defer server.Close()
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "credits-test-token", ExpiresAt: time.Now().Add(time.Hour)}
	got, err := queryTraeCNCredits(t.Context(), nil, account, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pools) != 2 {
		t.Fatalf("pools = %+v", got.Pools)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sources) != 1 || sources[0] != traeCNCreditsWorkReqSource {
		t.Fatalf("req_sources = %v, want a single Work-scope query", sources)
	}
	balance := account.TraeCNCreditsBalance()
	if balance.CodeRemaining != 0 || balance.WorkRemaining != 2000 || balance.ObservedAt.IsZero() {
		t.Fatalf("recorded balance = %+v", balance)
	}
}

func TestQueryTraeCNCreditsFallsBackToCodeScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(raw, "req_source").Int() == traeCNCreditsWorkReqSource {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"credentials-must-stay-private"}`)
			return
		}
		_, _ = io.WriteString(w, traeCNCreditsCodeOnlyFixture)
	}))
	defer server.Close()
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour)}
	got, err := queryTraeCNCredits(t.Context(), nil, account, server.URL)
	if err != nil {
		t.Fatalf("IDE scope must still serve the Code pool: %v", err)
	}
	code, _ := got.Pool(TraeCNCreditsPoolCode)
	if code.Total != 2650 || code.Remaining != 0 {
		t.Fatalf("code pool = %+v", code)
	}
	work, ok := got.Pool(TraeCNCreditsPoolWork)
	if !ok || !strings.Contains(work.Error, "Work") || work.Total != 0 {
		t.Fatalf("work pool must be marked unavailable instead of zero: %+v", work)
	}
	// 拿不到 Work 侧权益时不能把 Work 余额写成 0，否则 auto 会误判。
	if balance := account.TraeCNCreditsBalance(); !balance.ObservedAt.IsZero() {
		t.Fatalf("balance must stay unset without a Work-scope result: %+v", balance)
	}
}

func TestQueryTraeCNCreditsBoundsAndSanitizesErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http_error", 401, `{"error":"credentials-must-stay-private"}`},
		{"too_large", 200, strings.Repeat(" ", 1<<20) + traeCNCreditsPacksFixture},
		{"non_json", 200, "not-json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour)}
			_, err := queryTraeCNCredits(t.Context(), nil, account, server.URL)
			if err == nil || strings.Contains(err.Error(), "credentials-must-stay-private") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := queryTraeCNCredits(ctx, nil, account, "http://127.0.0.1:1"); err == nil {
		t.Fatal("canceled query succeeded")
	}
}

func TestQueryTraeCNCreditsRespectsResinEgress(t *testing.T) {
	previous := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previous) })
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, traeCNCreditsUsagePath) || r.Header.Get("X-Resin-Account") != "91234" || r.Header.Get("Authorization") != "Cloud-IDE-JWT AT" {
			t.Errorf("incorrect credit query route: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, traeCNCreditsPacksFixture)
	}))
	defer resin.Close()
	SetResinConfig(&ResinConfig{BaseURL: resin.URL + "/lease", PlatformName: "traecn-test"})
	account := &auth.Account{DBID: 91234, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour), ProxyURL: "http://127.0.0.1:1"}
	got, err := queryTraeCNCredits(t.Context(), nil, account, "https://trae-origin.invalid")
	if err != nil || len(got.Pools) != 2 {
		t.Fatalf("Resin query = %+v, %v", got, err)
	}
}
