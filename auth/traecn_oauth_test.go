package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTraeCNOAuthCallbackURLNormalizesBase(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://gw.example.com":             "https://gw.example.com" + TraeCNOAuthCallbackPath,
		"https://gw.example.com/":            "https://gw.example.com" + TraeCNOAuthCallbackPath,
		"https://gw.example.com/custom/path": "https://gw.example.com/custom/path",
		"http://127.0.0.1:8080":              "http://127.0.0.1:8080" + TraeCNOAuthCallbackPath,
	}
	for input, want := range cases {
		got, err := TraeCNOAuthCallbackURL(input)
		if err != nil || got != want {
			t.Errorf("TraeCNOAuthCallbackURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, invalid := range []string{"", "ftp://x", "not-a-url", "https://"} {
		if _, err := TraeCNOAuthCallbackURL(invalid); err == nil {
			t.Errorf("TraeCNOAuthCallbackURL(%q) should fail", invalid)
		}
	}
}

func TestTraeCNOAuthAuthorizationURLCarriesPKCEAndCallback(t *testing.T) {
	t.Parallel()
	raw, err := TraeCNOAuthAuthorizationURL("https://www.trae.cn", "trace-1", "https://gw.example.com/api/traecn/oauth/callback", "challenge-1", "machine-1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != TraeCNOAuthAuthorizationPath {
		t.Fatalf("path = %q, want %q", parsed.Path, TraeCNOAuthAuthorizationPath)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"login_version":         "1",
		"auth_from":             "trae",
		"login_channel":         "native_ide",
		"auth_type":             "local",
		"client_id":             TraeCNOAuthClientID,
		"redirect":              "0",
		"login_trace_id":        "trace-1",
		"code_challenge":        "challenge-1",
		"code_challenge_method": "S256",
		"machine_id":            "machine-1",
		"device_id":             "device-1",
		"x_app_version":         TraeCNOAuthAppVersion,
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// auth_callback_url 不转义：Trae 授权页按原样使用。
	if !strings.Contains(raw, "auth_callback_url=https://gw.example.com/api/traecn/oauth/callback") {
		t.Fatalf("callback url was encoded: %s", raw)
	}
}

func TestTraeCNOAuthPickStringWalksNestedPaths(t *testing.T) {
	t.Parallel()
	root := map[string]any{"data": map[string]any{"Result": map[string]any{"LoginHost": "https://www.trae.cn"}}}
	if got := traeCNOAuthPickString(root, "Result.LoginHost", "data.Result.LoginHost"); got != "https://www.trae.cn" {
		t.Fatalf("picked %q", got)
	}
	if got := traeCNOAuthPickString(root, "missing.key"); got != "" {
		t.Fatalf("missing path picked %q", got)
	}
}

func TestParseTraeCNOAuthCallbackAcceptsEveryShape(t *testing.T) {
	t.Parallel()
	full := "https://gw.example.com/api/traecn/oauth/callback?login_trace_id=trace-9&authCodeInfo=" +
		url.QueryEscape(`{"AuthCode":"code-1","ExpireAt":4102444800000}`) +
		"&userInfo=" + url.QueryEscape(`{"NonPlainTextEmail":"user@example.com"}`) +
		"&loginHost=" + url.QueryEscape("https://www.trae.cn") +
		"&loginRegion=cn&userTag=tag-1"
	payload, err := ParseTraeCNOAuthCallback(full)
	if err != nil {
		t.Fatal(err)
	}
	if payload.authCode != "code-1" || payload.loginTraceID != "trace-9" || payload.loginRegion != "cn" || payload.userTag != "tag-1" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.userInfo == nil || traeCNOAuthMapString(payload.userInfo, "NonPlainTextEmail") != "user@example.com" {
		t.Fatalf("userInfo = %+v", payload.userInfo)
	}

	plain, err := ParseTraeCNOAuthCallback("?authCode=code-2&loginTraceId=trace-2")
	if err != nil || plain.authCode != "code-2" || plain.loginTraceID != "trace-2" {
		t.Fatalf("query payload = %+v, err=%v", plain, err)
	}

	refresh, err := ParseTraeCNOAuthCallback("refreshToken=rt-1&x-cloudide-token=at-1")
	if err != nil || refresh.refreshToken != "rt-1" || refresh.cloudideToken != "at-1" {
		t.Fatalf("refresh payload = %+v, err=%v", refresh, err)
	}

	if _, err := ParseTraeCNOAuthCallback("https://gw.example.com/cb?login_trace_id=only"); err == nil {
		t.Fatal("callback without credentials should fail")
	}
	if _, err := ParseTraeCNOAuthCallback(""); err == nil {
		t.Fatal("empty callback should fail")
	}
}

// start + 回调 + 兑换整条链路：用一个假上游替换引导/兑换/用户信息三处域名。
func TestTraeCNOAuthStartAndCompleteAgainstFixture(t *testing.T) {
	var sawAuthCode, sawVerifier, sawDeviceInfo, sawAuthorizationHeader string
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case TraeCNOAuthGuidancePath:
			_, _ = w.Write([]byte(`{"Result":{"LoginHost":"` + "http://" + r.Host + `"}}`))
		case TraeCNOAuthAuthCodeExchangePth:
			var body struct {
				AuthCode     string         `json:"AuthCode"`
				CodeVerifier string         `json:"CodeVerifier"`
				DeviceInfo   map[string]any `json:"DeviceInfo"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sawAuthCode, sawVerifier = body.AuthCode, body.CodeVerifier
			encoded, _ := json.Marshal(body.DeviceInfo)
			sawDeviceInfo = string(encoded)
			_, _ = w.Write([]byte(`{"Result":{"Token":"at-oauth","RefreshToken":"rt-oauth","ExpiresAt":4102444800000,"UserID":"user-42"}}`))
		case TraeCNOAuthUserInfoPath:
			sawAuthorizationHeader = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"Result":{"NonPlainTextEmail":"oauth@example.com","UserID":"user-42"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	t.Setenv("TRAECN_OAUTH_GUIDANCE_HOSTS", fixture.URL)
	t.Setenv("TRAECN_OAUTH_AUTH_HOST", fixture.URL)

	start, err := StartTraeCNOAuth(context.Background(), "https://gw.example.com", "")
	if err != nil {
		t.Fatalf("StartTraeCNOAuth: %v", err)
	}
	if start.LoginID == "" || start.LoginTraceID == "" {
		t.Fatalf("start = %+v", start)
	}
	if !strings.Contains(start.VerificationURI, "/authorization") || !strings.Contains(start.VerificationURI, "code_challenge=") {
		t.Fatalf("verification uri = %q", start.VerificationURI)
	}
	if start.CallbackURL != "https://gw.example.com"+TraeCNOAuthCallbackPath {
		t.Fatalf("callback = %q", start.CallbackURL)
	}

	status, ok := TraeCNOAuthStatusFor(start.LoginID)
	if !ok || status.State != "pending" {
		t.Fatalf("initial status = %+v ok=%t", status, ok)
	}

	account, err := CompleteTraeCNOAuth(context.Background(), start.LoginTraceID,
		"?login_trace_id="+start.LoginTraceID+"&authCode=code-fixture&loginRegion=cn")
	if err != nil {
		t.Fatalf("CompleteTraeCNOAuth: %v", err)
	}
	if account.AccessToken != "at-oauth" || account.RefreshToken != "rt-oauth" || account.UserID != "user-42" {
		t.Fatalf("account = %+v", account)
	}
	if account.Email != "oauth@example.com" {
		t.Fatalf("email = %q", account.Email)
	}
	if account.LoginRegion != "cn" {
		t.Fatalf("login region = %q", account.LoginRegion)
	}
	if sawAuthCode != "code-fixture" || sawVerifier == "" {
		t.Fatalf("exchange payload authCode=%q verifier=%q", sawAuthCode, sawVerifier)
	}
	if !strings.Contains(sawDeviceInfo, "DevicePublicKey") || !strings.Contains(sawDeviceInfo, "MachineID") {
		t.Fatalf("device info = %s", sawDeviceInfo)
	}
	if !strings.HasPrefix(sawAuthorizationHeader, "Cloud-IDE-JWT ") {
		t.Fatalf("GetUserInfo authorization = %q", sawAuthorizationHeader)
	}

	status, ok = TraeCNOAuthStatusFor(start.LoginID)
	if !ok || status.State != "ready" || status.Account == nil || status.Account.RefreshToken != "rt-oauth" {
		t.Fatalf("final status = %+v ok=%t", status, ok)
	}
	// 幂等：重复提交返回同一账号。
	again, err := CompleteTraeCNOAuth(context.Background(), start.LoginID, "?authCode=other")
	if err != nil || again.RefreshToken != "rt-oauth" {
		t.Fatalf("idempotent complete = %+v err=%v", again, err)
	}
	CancelTraeCNOAuth(start.LoginID)
	if _, ok := TraeCNOAuthStatusFor(start.LoginID); ok {
		t.Fatal("cancelled session should be gone")
	}
}

// 授权失败要把错误留在会话里，前端轮询能看到原因。
func TestCompleteTraeCNOAuthRecordsExchangeFailure(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case TraeCNOAuthGuidancePath:
			_, _ = w.Write([]byte(`{"Result":{"LoginHost":"` + "http://" + r.Host + `"}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":4001,"message":"auth code expired"}`))
		}
	}))
	defer fixture.Close()
	t.Setenv("TRAECN_OAUTH_GUIDANCE_HOSTS", fixture.URL)
	t.Setenv("TRAECN_OAUTH_AUTH_HOST", fixture.URL)

	start, err := StartTraeCNOAuth(context.Background(), "https://gw.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteTraeCNOAuth(context.Background(), start.LoginID, "?authCode=expired"); err == nil {
		t.Fatal("expired auth code should fail")
	}
	status, ok := TraeCNOAuthStatusFor(start.LoginID)
	if !ok || status.State != "error" || !strings.Contains(status.Message, "4001") {
		t.Fatalf("status = %+v ok=%t", status, ok)
	}
}
