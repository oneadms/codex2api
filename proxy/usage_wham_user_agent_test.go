package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 在同一账号和连接池上连续更新后台设置，校验 Wham 与 Responses 的实际出站 UA。
func TestWhamRequestsUseResponsesUserAgentAfterSettingsReload(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	endpoints := []struct {
		path string
		call func(context.Context, *auth.Account, string) (*http.Response, error)
	}{
		{
			path: "/backend-api/wham/usage",
			call: func(ctx context.Context, account *auth.Account, url string) (*http.Response, error) {
				_, resp, err := queryWhamUsageWithURL(ctx, account, "", url)
				return resp, err
			},
		},
		{
			path: "/backend-api/wham/rate-limit-reset-credits",
			call: func(ctx context.Context, account *auth.Account, url string) (*http.Response, error) {
				_, resp, err := queryWhamResetCreditsWithURL(ctx, account, "", url)
				return resp, err
			},
		},
		{
			path: "/backend-api/wham/rate-limit-reset-credits/consume",
			call: func(ctx context.Context, account *auth.Account, url string) (*http.Response, error) {
				_, resp, err := consumeResetCreditWithURL(ctx, account, "", url, "test-redeem-id")
				return resp, err
			},
		},
		{
			path: "/backend-api/wham/analytics/daily-workspace-usage-counts",
			call: func(ctx context.Context, account *auth.Account, url string) (*http.Response, error) {
				_, resp, err := queryWhamDailyUsageWithURL(ctx, account, "", url, "2026-09-08", "2026-09-09")
				return resp, err
			},
		},
		{
			path: "/backend-api/wham/usage/daily-token-usage-breakdown",
			call: func(ctx context.Context, account *auth.Account, url string) (*http.Response, error) {
				_, resp, err := queryWhamDailyTokenBreakdownWithURL(ctx, account, "", url, "2026-09-08", "2026-09-09")
				return resp, err
			},
		},
	}

	const rawUA = "codex-tui/0.147.0 (Mac OS 15.4.0; arm64) tmux/3.5a (codex-tui; 0.147.0)"
	const structuredConfig = `{"client_version":"0.142.0","os_name":"Linux","os_version":"Unknown","arch":"x86_64","terminal":"tmux/3.5a"}`
	for _, tc := range []struct {
		name          string
		mode          string
		config        string
		customHeaders map[string]string
		wantUA        string
	}{
		{name: "raw_force", mode: ClientCompatModeForce, config: `{"raw_user_agent":"` + rawUA + `"}`, wantUA: rawUA},
		{name: "raw_auto", mode: ClientCompatModeAuto, config: `{"raw_user_agent":"` + rawUA + `"}`, wantUA: rawUA},
		{name: "raw_preserve", mode: ClientCompatModePreserve, config: `{"raw_user_agent":"` + rawUA + `"}`, wantUA: rawUA},
		{name: "raw_changed", mode: ClientCompatModeForce, config: `{"raw_user_agent":"my-router/2.0"}`, wantUA: "my-router/2.0"},
		{
			name: "structured_force", mode: ClientCompatModeForce, config: structuredConfig,
			wantUA: "codex-tui/0.142.0 (Linux Unknown; x86_64) tmux/3.5a (codex-tui; 0.142.0)",
		},
		{
			name: "structured_auto_floor", mode: ClientCompatModeAuto, config: structuredConfig,
			wantUA: "codex-tui/0.160.0 (Linux Unknown; x86_64) tmux/3.5a (codex-tui; 0.160.0)",
		},
		{
			name: "account_override", mode: ClientCompatModeForce, config: `{"raw_user_agent":"` + rawUA + `"}`,
			customHeaders: map[string]string{"uSeR-aGeNt": "account-agent/3.0"}, wantUA: "account-agent/3.0",
		},
		{name: "cleared_force", mode: ClientCompatModeForce, config: `{}`},
		{name: "cleared_preserve", mode: ClientCompatModePreserve, config: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
				ClientCompatMode:      tc.mode,
				CodexMinCLIVersion:    "0.160.0",
				CodexSyncedCLIVersion: "0.153.4",
				CodexUserAgentConfig:  tc.config,
			})
			account := &auth.Account{
				DBID: 964502, AccountID: "ua-account", AccessToken: "test-access-token",
				CustomHeaders: tc.customHeaders,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/backend-api/codex/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			applyCodexRequestHeaders(req, account, account.AccessToken, "", "", nil, http.Header{"User-Agent": {"curl/8.7.1"}})
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			responsesUA := (<-received).Get("User-Agent")
			if responsesUA == "" || (tc.wantUA != "" && responsesUA != tc.wantUA) {
				t.Fatalf("Responses User-Agent = %q, want %q", responsesUA, tc.wantUA)
			}

			for _, endpoint := range endpoints {
				t.Run(endpoint.path[1:], func(t *testing.T) {
					resp, err := endpoint.call(ctx, account, server.URL+endpoint.path)
					if resp != nil && resp.Body != nil {
						_ = resp.Body.Close()
					}
					if err != nil {
						t.Fatal(err)
					}
					headers := <-received
					if got := headers.Get("User-Agent"); got != responsesUA {
						t.Errorf("Wham User-Agent = %q, Responses User-Agent = %q", got, responsesUA)
					}
					if got := headers.Get("Accept"); got != "application/json" {
						t.Errorf("Accept = %q, want application/json", got)
					}
					if got := headers.Get("Authorization"); got != "Bearer test-access-token" {
						t.Errorf("Authorization = %q, want account access token", got)
					}
				})
			}
		})
	}
}
