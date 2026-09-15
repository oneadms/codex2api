package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// 仅保留抓包中的计费标志和汇总，不把用户身份或权益包标识带入测试。
const traeCNCreditsFixture = `{"is_credits_billing":true,"is_dollar_usage_billing":false,"usage_summary":{"total_amount":1100,"consumed_amount":611.42,"consumption_ratio":0.5558363636363636}}`

func TestParseTraeCNCredits(t *testing.T) {
	for _, tc := range []struct {
		name, payload                   string
		total, used, remaining, percent float64
	}{
		{"capture_summary", traeCNCreditsFixture, 1100, 611.42, 488.58, 55.58363636363636},
		{"empty_balance", `{"is_credits_billing":true,"usage_summary":{"total_amount":0,"consumed_amount":0}}`, 0, 0, 0, 0},
		{"exhausted", `{"is_credits_billing":true,"usage_summary":{"total_amount":100,"consumed_amount":100}}`, 100, 100, 0, 100},
		{"overdrawn", `{"is_credits_billing":true,"usage_summary":{"total_amount":100,"consumed_amount":100.5}}`, 100, 100.5, 0, 100},
		{"zero_total_with_usage", `{"is_credits_billing":true,"usage_summary":{"total_amount":0,"consumed_amount":1}}`, 0, 1, 0, 100},
		{"numeric_strings", `{"code":0,"is_credits_billing":true,"usage_summary":{"total_amount":"1100","consumed_amount":"611.42","consumption_ratio":99}}`, 1100, 611.42, 488.58, 55.58363636363636},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			got, err := parseTraeCNCredits([]byte(tc.payload), now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Total != tc.total || got.Used != tc.used || math.Abs(got.Remaining-tc.remaining) > 1e-8 || math.Abs(got.UsedPercent-tc.percent) > 1e-8 || !got.UpdatedAt.Equal(now) {
				t.Fatalf("unexpected snapshot: %+v", got)
			}
		})
	}
}

func TestParseTraeCNCreditsRejectsMissingOrDifferentBilling(t *testing.T) {
	for _, payload := range []string{
		`<html>upstream error</html>`,
		`{}`,
		`{"is_credits_billing":false,"usage_summary":{"total_amount":10,"consumed_amount":1}}`,
		`{"is_credits_billing":true,"is_dollar_usage_billing":true,"usage_summary":{"total_amount":10,"consumed_amount":1}}`,
		`{"is_credits_billing":true,"usage_summary":{"total_amount":100}}`,
		`{"is_credits_billing":true,"usage_summary":{"total_amount":100,"consumed_amount":null}}`,
		`{"is_credits_billing":true,"usage_summary":{"total_amount":-1,"consumed_amount":0}}`,
		`{"is_credits_billing":true,"usage_summary":{"total_amount":"NaN","consumed_amount":0}}`,
		`{"is_credits_billing":true,"usage_summary":{"total_amount":100,"consumed_amount":"Infinity"}}`,
		`{"is_credits_billing":true,"usage_summary":{"total_amount":100,"consumed_amount":"unknown"}}`,
		`{"code":3004,"is_credits_billing":true,"usage_summary":{"total_amount":100,"consumed_amount":0}}`,
	} {
		if got, err := parseTraeCNCredits([]byte(payload), time.Now()); err == nil || !got.UpdatedAt.IsZero() {
			t.Fatalf("invalid response accepted: %s -> %+v, %v", payload, got, err)
		}
	}
}

func TestQueryTraeCNCreditsUsesMarketIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != traeCNCreditsUsagePath {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT credits-test-token" || r.Header.Get("x-market-client-id") != auth.TraeCNMarketClientID || r.Header.Get("x-device-id") == "" || r.Header.Get("x-ide-token") != "" {
			t.Error("market identity missing or inference identity used")
		}
		raw, _ := io.ReadAll(r.Body)
		if !gjson.GetBytes(raw, "require_usage").Bool() || gjson.GetBytes(raw, "req_source").Int() != 1 {
			t.Errorf("request body = %s", raw)
		}
		_, _ = io.WriteString(w, traeCNCreditsFixture)
	}))
	defer server.Close()
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "credits-test-token", ExpiresAt: time.Now().Add(time.Hour)}
	got, err := queryTraeCNCredits(t.Context(), nil, account, server.URL)
	if err != nil || got.Total != 1100 || got.Used != 611.42 {
		t.Fatalf("query = %+v, %v", got, err)
	}
}

func TestQueryTraeCNCreditsBoundsAndSanitizesErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http_error", 401, `{"error":"credentials-must-stay-private"}`},
		{"too_large", 200, strings.Repeat(" ", 1<<20) + traeCNCreditsFixture},
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
		_, _ = io.WriteString(w, traeCNCreditsFixture)
	}))
	defer resin.Close()
	SetResinConfig(&ResinConfig{BaseURL: resin.URL + "/lease", PlatformName: "traecn-test"})
	account := &auth.Account{DBID: 91234, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour), ProxyURL: "http://127.0.0.1:1"}
	got, err := queryTraeCNCredits(t.Context(), nil, account, "https://trae-origin.invalid")
	if err != nil || got.Total != 1100 {
		t.Fatalf("Resin query = %+v, %v", got, err)
	}
}
