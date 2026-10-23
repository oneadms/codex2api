package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestResinChannelPlatforms(t *testing.T) {
	previous, previousEnabled := GetResinConfig(), auth.ResinEgressEnabled()
	previousDecorator := auth.ResinRequestResolver
	t.Cleanup(func() {
		SetResinConfig(previous)
		auth.SetResinEgressEnabled(previousEnabled)
		auth.ResinRequestResolver = previousDecorator
	})
	auth.ResinRequestResolver = ResolveResinAuthRoute
	for _, tc := range []struct {
		name, shared, codex, traecn, wantCodex, wantTraeCN string
	}{
		{name: "shared", shared: "shared", wantCodex: "shared", wantTraeCN: "shared"},
		{name: "independent", shared: "shared", codex: "codex", traecn: "traecn", wantCodex: "codex", wantTraeCN: "traecn"},
		{name: "fallback", shared: "shared", codex: "codex", wantCodex: "codex", wantTraeCN: "shared"},
		{name: "only codex", codex: "codex", wantCodex: "codex"},
		{name: "only traecn", traecn: "traecn", wantTraeCN: "traecn"},
		{name: "normalization", shared: " shared,shared ", codex: " , ; ", traecn: " trae,trae ", wantCodex: "shared", wantTraeCN: "trae"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ResinConfig{BaseURL: "http://resin.test/token", PlatformName: tc.shared, CodexPlatformName: tc.codex, TraeCNPlatformName: tc.traecn}
			if err := ValidateResinConfig(cfg); err != nil {
				t.Fatal(err)
			}
			SetResinConfig(cfg)
			if IsResinEnabled() != (tc.wantCodex != "") || auth.ResinEgressEnabled() != (tc.wantCodex != "") {
				t.Fatal("Codex 出口标记与独立平台不一致")
			}
			for _, channel := range []struct {
				kind, want string
				summary    CodexEgressSummary
			}{
				{want: tc.wantCodex, summary: CurrentCodexEgressSummary()},
				{kind: auth.UpstreamTraeCN, want: tc.wantTraeCN, summary: CurrentTraeCNEgressSummary()},
			} {
				account := &auth.Account{DBID: 42, UpstreamType: channel.kind, AccessToken: "token"}
				ctx := auth.WithResinChannel(t.Context(), channel.kind)
				for _, ws := range []bool{false, true} {
					target := "https://upstream.test/request"
					if ws {
						target = "wss://upstream.test/request"
					}
					egress := ResolveCodexRequestEgress(t.Context(), account, target, "http://proxy.test", ws)
					wantURL := target
					if channel.want != "" {
						wantURL = "http://resin.test/token/" + channel.want + "/https/upstream.test/request"
						if ws {
							wantURL = strings.Replace(wantURL, "http://", "ws://", 1)
						}
					}
					if egress.URL != wantURL || egress.ViaResin() != (channel.want != "") {
						t.Fatalf("channel=%q ws=%v route=%+v, want %s", channel.kind, ws, egress, wantURL)
					}
				}
				target := "https://upstream.test/token"
				route, err := auth.ResolveResinRequest(ctx, target, "42")
				if err != nil {
					t.Fatal(err)
				}
				got, enabled := route.URL, route.Enabled()
				want := target
				if channel.want != "" {
					want = "http://resin.test/token/" + channel.want + "/https/upstream.test/token"
				}
				if got != want || enabled != (channel.want != "") || channel.summary.ResinPlatformName != channel.want || channel.summary.ResinEnabled != enabled {
					t.Fatalf("channel=%q auth=%s summary=%+v", channel.kind, got, channel.summary)
				}
			}
		})
	}
}

// 刷新令牌期间热更新配置，推理仍须使用入口按会话固定的 TRAE CN 平台。
func TestTraeCNResinPlatformStaysWithRefreshAndInference(t *testing.T) {
	previous, previousEnabled := GetResinConfig(), auth.ResinEgressEnabled()
	t.Cleanup(func() {
		SetResinConfig(previous)
		auth.SetResinEgressEnabled(previousEnabled)
	})
	paths := make(chan string, 4)
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		if r.Header.Get("X-Resin-Account") != "91401" {
			t.Errorf("unexpected account %q", r.Header.Get("X-Resin-Account"))
		}
		if strings.HasSuffix(r.URL.Path, auth.TraeCNExchangePath) {
			SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:1", TraeCNPlatformName: "changed"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"new-at","refreshToken":"new-rt","expiresIn":3600}`)
			return
		}
		if r.Header.Get("x-ide-token") != "new-at" {
			t.Error("推理未使用刷新后的令牌")
		}
		traeCNResinTestResponse(w)
	}))
	defer resin.Close()
	SetResinConfig(&ResinConfig{BaseURL: resin.URL + "/lease", PlatformName: "shared", CodexPlatformName: "codex1,codex2", TraeCNPlatformName: "trae1,trae2"})
	identity := requestSessionIdentity{affinityID: "session-42", hasStableAffinity: true}
	platforms := resinPlatformsForSessionIdentity(t.Context(), identity, 7)
	ctx := withResinPlatforms(t.Context(), platforms)
	account := &auth.Account{DBID: 91401, UpstreamType: auth.UpstreamTraeCN, AccessToken: "expired", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour), ProxyURL: "http://127.0.0.1:1"}
	body := []byte(`{"model":"deepseek-v3","input":"hi","stream":true}`)
	resp, err := ExecuteTraeCNRequest(ctx, account, GrokProtocolResponses, body, body, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("requests=%d, want refresh and inference", len(paths))
	}
	for range 2 {
		path := <-paths
		if !strings.HasPrefix(path, "/lease/"+platforms.traecn+"/") {
			t.Fatalf("request used another channel or updated platform: %s", path)
		}
	}
	codexCtx := withResinPlatforms(WithResinConfig(t.Context(), &ResinConfig{BaseURL: resin.URL, CodexPlatformName: "codex1,codex2"}), platforms)
	egress := ResolveCodexEgressForContext(codexCtx, &auth.Account{DBID: 91402}, "https://chatgpt.com/backend-api/codex/responses", "")
	if !strings.Contains(egress.URL, "/"+platforms.codex+"/https/chatgpt.com/") {
		t.Fatalf("Codex 平台被 TRAE CN 请求覆盖: %s", egress.URL)
	}
}

func TestFetchTraeCNModelsUsesChannelResin(t *testing.T) {
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lease/traecn/https/trae-origin.invalid"+traeCNModelsBatchDetailPath || r.Header.Get("X-Resin-Account") != "91403" {
			t.Errorf("模型查询平台或账号错误: %s %q", r.URL.Path, r.Header.Get("X-Resin-Account"))
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"glm-5.3-flash","config_switch":true}]}]}`)
	}))
	defer resin.Close()
	ctx := WithResinConfig(t.Context(), &ResinConfig{BaseURL: resin.URL + "/lease", PlatformName: "shared", CodexPlatformName: "codex", TraeCNPlatformName: "traecn"})
	account := &auth.Account{DBID: 91403, UpstreamType: auth.UpstreamTraeCN, AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: "https://trae-origin.invalid", ProxyURL: "http://127.0.0.1:1"}
	models, err := FetchTraeCNModels(ctx, account, "")
	if err != nil || !containsFold(models, "glm-5.3-flash") {
		t.Fatalf("models=%v err=%v", models, err)
	}
}
