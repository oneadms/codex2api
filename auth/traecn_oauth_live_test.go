package auth

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// 真实上游联调：默认跳过，用 TRAECN_LIVE_OAUTH=1 打开。
//
//	go test ./auth/ -run TestLiveTraeCNOAuthGuidance -v
//
// 只验证引导接口与授权链接（登录需要人工在浏览器完成，无法自动化）。
func TestLiveTraeCNOAuthGuidance(t *testing.T) {
	if strings.TrimSpace(os.Getenv("TRAECN_LIVE_OAUTH")) != "1" {
		t.Skip("set TRAECN_LIVE_OAUTH=1 to hit the live Trae CN guidance endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	loginHost, errs := RequestTraeCNLoginHost(ctx, "codex2api-live-probe", "")
	t.Logf("login host = %q (errors: %s)", loginHost, errs)
	if loginHost == "" {
		t.Fatal("guidance returned an empty login host")
	}

	verifier, challenge, err := generateTraeCNOAuthPKCE()
	if err != nil || verifier == "" || challenge == "" {
		t.Fatalf("pkce = %q/%q err=%v", verifier, challenge, err)
	}
	profile := traeCNDeviceProfileForSeed("codex2api-live-probe")
	link, err := TraeCNOAuthAuthorizationURL(loginHost, "codex2api-trace", "https://gw.example.com/api/traecn/oauth/callback", challenge, profile.MachineID, profile.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for _, key := range []string{"login_trace_id", "client_id", "code_challenge", "auth_callback_url", "machine_id", "device_id"} {
		if query.Get(key) == "" {
			t.Errorf("authorization link missing %s: %s", key, link)
		}
	}
	t.Logf("authorization link host = %s, client_id = %s", parsed.Host, query.Get("client_id"))
}
