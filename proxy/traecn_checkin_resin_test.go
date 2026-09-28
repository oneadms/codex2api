package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

// 状态查询、领取及复查必须携带同一账号标识，且 Resin 优先于账号代理和临时代理。
func TestRunTraeCNCheckinRoutesThroughResin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, platform, wantPlatform, proxyOverride string
	}{
		{name: "default_platform", wantPlatform: "p1"},
		{name: "selected_platform", platform: "p2", wantPlatform: "p2", proxyOverride: "http://127.0.0.1:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var statusCalls, claimCalls atomic.Int32
			prefix := "/lease/" + tc.wantPlatform + "/https/api.trae.cn"
			resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("X-Resin-Account") != "91306" {
					t.Errorf("签到路由或账号标识错误: %s %s, account=%q", r.Method, r.URL.Path, r.Header.Get("X-Resin-Account"))
				}
				if r.Header.Get("Authorization") != "Cloud-IDE-JWT AT" || r.Header.Get("x-market-client-id") != traeCNCheckinMarketClientID {
					t.Error("Resin 请求缺少签到认证或市场客户端标识")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != `{"req_source":1}` {
					t.Errorf("签到请求体错误: %s, %v", body, err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case prefix + traeCNCheckinStatusPath:
					if statusCalls.Add(1) == 1 {
						_, _ = io.WriteString(w, `{"code":0,"enable":true,"checked_in":false}`)
					} else {
						_, _ = io.WriteString(w, `{"code":0,"enable":true,"checked_in":true,"credits":150,"extra_credits":50}`)
					}
				case prefix + traeCNCheckinClaimPath:
					claimCalls.Add(1)
					_, _ = io.WriteString(w, `{"code":0,"message":"success"}`)
				default:
					t.Errorf("签到请求使用了错误的 Resin 路径: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer resin.Close()
			ctx := WithResinConfig(t.Context(), &ResinConfig{BaseURL: resin.URL + "/lease", PlatformName: "p1,p2"})
			ctx = WithResinPlatform(ctx, tc.platform)
			account := &auth.Account{DBID: 91306, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour), ProxyURL: "http://127.0.0.1:1"}
			outcome, err := RunTraeCNCheckin(ctx, nil, account, tc.proxyOverride)
			if err != nil {
				t.Fatalf("Resin 签到失败: %v", err)
			}
			if !outcome.Claimed || !outcome.CheckedIn || outcome.Status.Credits != 150 || outcome.Status.Extra != 50 || outcome.Egress != "resin" {
				t.Fatalf("签到结果错误: %+v", outcome)
			}
			if statusCalls.Load() != 2 || claimCalls.Load() != 1 {
				t.Fatalf("status=%d claim=%d，期望 2/1", statusCalls.Load(), claimCalls.Load())
			}
		})
	}
}

// 后台任务从全局配置取快照；刷新期间热更新不能让后续签到绕回直连。
func TestRunTraeCNCheckinRefreshKeepsResinSnapshot(t *testing.T) {
	previous, previousEnabled := GetResinConfig(), auth.ResinEgressEnabled()
	t.Cleanup(func() {
		SetResinConfig(previous)
		auth.SetResinEgressEnabled(previousEnabled)
	})
	var refreshCalls, statusCalls, claimCalls atomic.Int32
	prefix := "/lease/p1/https/api.trae.cn"
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Resin-Account") != "91307" {
			t.Errorf("账号标识错误: %q", r.Header.Get("X-Resin-Account"))
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == prefix+auth.TraeCNExchangePath {
			refreshCalls.Add(1)
			SetResinConfig(nil)
			_, _ = io.WriteString(w, `{"token":"new-at","refreshToken":"new-rt","expiresIn":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT new-at" {
			t.Error("签到未使用刷新后的访问令牌")
		}
		switch r.URL.Path {
		case prefix + traeCNCheckinStatusPath:
			if statusCalls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"code":0,"enable":true,"checked_in":false}`)
			} else {
				_, _ = io.WriteString(w, `{"code":0,"enable":true,"checked_in":true,"credits":150}`)
			}
		case prefix + traeCNCheckinClaimPath:
			claimCalls.Add(1)
			_, _ = io.WriteString(w, `{"code":0}`)
		default:
			t.Errorf("请求未保留 Resin 配置快照: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer resin.Close()
	SetResinConfig(&ResinConfig{BaseURL: resin.URL + "/lease", PlatformName: "p1,p2"})
	account := &auth.Account{DBID: 91307, UpstreamType: auth.UpstreamTraeCN, AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)}
	store := &auth.Store{}
	store.SetProxyPoolEnabled(true)
	outcome, err := RunTraeCNCheckin(t.Context(), store, account, "")
	if err != nil || !outcome.Claimed || !outcome.CheckedIn || outcome.Egress != "resin" {
		t.Fatalf("刷新后签到结果错误: %+v, %v", outcome, err)
	}
	if refreshCalls.Load() != 1 || statusCalls.Load() != 2 || claimCalls.Load() != 1 {
		t.Fatalf("refresh=%d status=%d claim=%d，期望 1/2/1", refreshCalls.Load(), statusCalls.Load(), claimCalls.Load())
	}
}

// Resin 返回错误或重定向时保留实际错误，不能自动切换到本机出口再次领取。
func TestRunTraeCNCheckinResinFailuresDoNotFallBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, wantError string }{
		{name: "http_error", wantError: "traecn checkin status: HTTP 502"},
		{name: "business_error", wantError: "traecn checkin claim: 当前参与用户太多，请稍后再试 (code 9074)"},
		{name: "redirect", wantError: "traecn checkin status: HTTP 307"},
		{name: "transport_error", wantError: "traecn checkin status: context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var directCalls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				directCalls.Add(1)
				_, _ = io.WriteString(w, `{"code":0,"checked_in":true}`)
			}))
			defer origin.Close()
			resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch tc.name {
				case "http_error":
					http.Error(w, "Bad gateway", http.StatusBadGateway)
				case "redirect":
					http.Redirect(w, r, origin.URL+traeCNCheckinStatusPath, http.StatusTemporaryRedirect)
				case "business_error":
					if strings.HasSuffix(r.URL.Path, traeCNCheckinStatusPath) {
						_, _ = io.WriteString(w, `{"code":0,"enable":true,"checked_in":false}`)
					} else {
						_, _ = io.WriteString(w, `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`)
					}
				}
			}))
			defer resin.Close()
			ctx := WithResinConfig(t.Context(), &ResinConfig{BaseURL: resin.URL + "/private-lease-token", PlatformName: "p1"})
			if tc.name == "transport_error" {
				canceledCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceledCtx
			}
			account := &auth.Account{DBID: 91308, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour)}
			outcome, err := runTraeCNCheckin(ctx, nil, account, "", origin.URL)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) || outcome.Egress != "resin" || outcome.Claimed {
				t.Fatalf("失败结果错误: %+v, %v", outcome, err)
			}
			if strings.Contains(err.Error(), "private-lease-token") {
				t.Fatalf("错误信息泄露了 Resin 访问令牌: %v", err)
			}
			if directCalls.Load() != 0 {
				t.Fatalf("Resin 失败后出现 %d 次直连", directCalls.Load())
			}
		})
	}
}

func TestRunTraeCNCheckinWithoutResinUsesAccountProxy(t *testing.T) {
	t.Parallel()
	for _, override := range []bool{false, true} {
		var proxyCalls atomic.Int32
		forwardProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxyCalls.Add(1)
			if r.URL.Host != "trae-origin.invalid" || r.URL.Path != traeCNCheckinStatusPath || r.Header.Get("X-Resin-Account") != "" {
				t.Errorf("普通代理请求错误: %s, resin-account=%q", r.URL, r.Header.Get("X-Resin-Account"))
			}
			_, _ = io.WriteString(w, `{"code":0,"checked_in":true,"credits":150}`)
		}))
		account := &auth.Account{DBID: 91309, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour), ProxyURL: forwardProxy.URL}
		proxyOverride := ""
		if override {
			account.ProxyURL = "http://127.0.0.1:1"
			proxyOverride = forwardProxy.URL
		}
		ctx := WithResinConfig(t.Context(), nil)
		outcome, err := runTraeCNCheckin(ctx, nil, account, proxyOverride, "http://trae-origin.invalid")
		forwardProxy.Close()
		if err != nil || !outcome.CheckedIn || outcome.Egress != strings.TrimPrefix(forwardProxy.URL, "http://") || proxyCalls.Load() != 1 {
			t.Fatalf("普通代理签到结果错误（override=%t）: %+v, %v, calls=%d", override, outcome, err, proxyCalls.Load())
		}
	}
}

func TestRunTraeCNCheckinTransientAccountUsesDirectRoute(t *testing.T) {
	t.Parallel()
	var directCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		if r.URL.Path != traeCNCheckinStatusPath || r.Header.Get("X-Resin-Account") != "" {
			t.Errorf("临时账号误用了 Resin 身份: %s, %q", r.URL.Path, r.Header.Get("X-Resin-Account"))
		}
		_, _ = io.WriteString(w, `{"code":0,"checked_in":true,"credits":150}`)
	}))
	defer origin.Close()
	ctx := WithResinConfig(t.Context(), &ResinConfig{BaseURL: "http://127.0.0.1:1/lease", PlatformName: "p1"})
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour)}
	outcome, err := runTraeCNCheckin(ctx, nil, account, "", origin.URL)
	if err != nil || !outcome.CheckedIn || outcome.Egress != "none" || directCalls.Load() != 1 {
		t.Fatalf("临时账号签到结果错误: %+v, %v, calls=%d", outcome, err, directCalls.Load())
	}
}
