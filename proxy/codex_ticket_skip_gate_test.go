package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

func TestSkippedHarvestAccountMissingTicketGate(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	settings := auth.ConfiguredCodexTicketSettings()
	settings.TargetLength = 780
	settings.Models = []string{"gpt-6-astra", "gpt-6.1-sol"}
	auth.SetConfiguredCodexTicketSettings(settings)

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-only" {
			t.Error("跳过采票后仍应使用目标账号自己的凭据")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upstream.Close)
	previousResin, previousWS := resinCfg.Load(), WebsocketExecuteFunc
	t.Cleanup(func() {
		resinCfg.Store(previousResin)
		WebsocketExecuteFunc = previousWS
	})
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "skip-harvest-test"})
	poolKey := fmt.Sprintf("resin|%d", account.ID())
	clientPool.Delete(poolKey)
	t.Cleanup(func() { clientPool.Delete(poolKey) })
	WebsocketExecuteFunc = func(_ context.Context, a *auth.Account, _ []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		calls.Add(1)
		if a != account {
			t.Error("跳过采票不应切换目标账号")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}

	// 使用保存后的跳过名单，覆盖勾选、取消及测试连接共用的实际出站入口。
	for _, stage := range []struct {
		name    string
		skipped bool
	}{
		{"默认拦截", false},
		{"跳过采票", true},
		{"取消跳过", false},
	} {
		t.Run(stage.name, func(t *testing.T) {
			scope := harvest.DefaultScope()
			if stage.skipped {
				scope.SkippedAccountIDs = []int64{account.ID()}
			}
			if err := m.SaveScope(t.Context(), scope); err != nil {
				t.Fatal(err)
			}
			if stage.skipped && harvestAccountEligible(account, scope, false) {
				t.Fatal("跳过的账号不能重新进入采票范围")
			}
			other := &auth.Account{DBID: account.ID() + 1, PlanType: "plus"}
			if _, blocked := CodexTicketGateBlocked(t.Context(), other, "", settings.Models[0]); !blocked {
				t.Fatal("跳过名单不能豁免其他无票账号")
			}
			for _, model := range settings.Models {
				for _, protocol := range []string{"sse", "websocket", "compact"} {
					for _, probe := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/probe=%t", model, protocol, probe), func(t *testing.T) {
							ctx := t.Context()
							if probe {
								ctx = WithCodexSyntheticProbe(ctx)
							}
							body := []byte(fmt.Sprintf(`{"model":%q,"input":"hi"}`, model))
							before := calls.Load()
							var response *http.Response
							var err error
							if protocol == "compact" {
								response, err = ExecuteCompactRequest(ctx, account, body, "", "", "", nil, nil)
							} else {
								response, err = ExecuteRequest(ctx, account, body, "", "", "", nil, nil, protocol == "websocket")
							}
							if response != nil && response.Body != nil {
								_, _ = io.Copy(io.Discard, response.Body)
								_ = response.Body.Close()
							}
							if stage.skipped {
								if err != nil || response == nil || response.StatusCode != http.StatusOK || calls.Load() != before+1 {
									t.Fatalf("跳过账号应正常出站: err=%v calls=%d", err, calls.Load()-before)
								}
							} else {
								var ticketErr *Error
								if !errors.As(err, &ticketErr) || ticketErr.Code != ErrorCodeCodexTicketUnavailable || calls.Load() != before {
									t.Fatalf("未跳过账号应在出站前拦截: err=%v calls=%d", err, calls.Load()-before)
								}
							}
						})
					}
				}
			}
		})
	}
}
