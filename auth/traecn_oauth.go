package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Trae CN OAuth（IDE 授权码 + PKCE）。流程与本地 Trae 客户端一致：
//
//  1. POST {guidance host}/cloudide/api/v3/trae/GetLoginGuidance 取登录域 LoginHost；
//  2. 拼 {LoginHost}/authorization?...&code_challenge=...&auth_callback_url=... 让用户在
//     浏览器里登录，登录完成后 Trae 把 authCode（或 authCodeInfo / refreshToken）
//     带回 auth_callback_url；
//  3. POST https://api.trae.cn/trae/api/v3/oauth/ExchangeToken
//     {ClientID, AuthCode, CodeVerifier, DeviceInfo, IDEVersion} 换成长期凭据；
//  4. POST https://api.trae.cn/cloudide/api/v3/trae/GetUserInfo 取账号信息。
//
// 回调地址由调用方给出（网关自己的公网地址），所以浏览器和网关可以不在同一台机器上。
const (
	TraeCNOAuthGuidancePath        = "/cloudide/api/v3/trae/GetLoginGuidance"
	TraeCNOAuthAuthorizationPath   = "/authorization"
	TraeCNOAuthAuthCodeExchangePth = "/trae/api/v3/oauth/ExchangeToken"
	TraeCNOAuthUserInfoPath        = "/cloudide/api/v3/trae/GetUserInfo"
	// TraeCNOAuthPluginVersion / AppVersion / AppType 是授权页要求的客户端标识。
	// Trae 客户端 3.5.54 起才支持 auth_type=local 的授权码流程，低于该版本会被拒。
	TraeCNOAuthPluginVersion = "local"
	TraeCNOAuthAppVersion    = "3.5.54"
	TraeCNOAuthAppType       = "stable"
	// TraeCNOAuthSessionTTL 是一次授权会话的有效期，与客户端保持一致（10 分钟）。
	TraeCNOAuthSessionTTL = 10 * time.Minute
	// TraeCNOAuthCallbackPath 是网关自己的回调路径（管理台默认用它拼回调地址）。
	TraeCNOAuthCallbackPath = "/api/traecn/oauth/callback"
	TraeCNOAuthTimeout      = 20 * time.Second
)

// traeCNOAuthGuidanceHosts 是登录引导域名，按顺序尝试；全部失败时回落到 www.trae.cn。
// TRAECN_OAUTH_GUIDANCE_HOSTS（逗号分隔）可覆盖，便于自建反代/测试。
var traeCNOAuthGuidanceHosts = []string{
	"https://api.trae.cn",
	"https://api.trae.com.cn",
	"https://www.trae.cn",
}

// TraeCNOAuthAuthHost 是授权码兑换与账号信息接口的域名。TRAECN_OAUTH_AUTH_HOST
// 可覆盖（调试/测试用），默认 api.trae.cn。
func TraeCNOAuthAuthHost() string {
	return firstTraeCNEnv("TRAECN_OAUTH_AUTH_HOST")
}

func traeCNOAuthResolvedAuthHost() string {
	if host := strings.TrimRight(strings.TrimSpace(TraeCNOAuthAuthHost()), "/"); host != "" {
		return host
	}
	return TraeCNAuthHost
}

func traeCNOAuthResolvedGuidanceHosts() []string {
	raw := strings.TrimSpace(firstTraeCNEnv("TRAECN_OAUTH_GUIDANCE_HOSTS"))
	if raw == "" {
		return traeCNOAuthGuidanceHosts
	}
	hosts := make([]string, 0, 3)
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimRight(strings.TrimSpace(part), "/"); part != "" {
			hosts = append(hosts, part)
		}
	}
	if len(hosts) == 0 {
		return traeCNOAuthGuidanceHosts
	}
	return hosts
}

// TraeCNOAuthStart 是一次授权的启动信息，前端据此打开授权页并轮询结果。
type TraeCNOAuthStart struct {
	LoginID         string `json:"login_id"`
	LoginTraceID    string `json:"login_trace_id"`
	VerificationURI string `json:"verification_uri"`
	CallbackURL     string `json:"callback_url"`
	LoginHost       string `json:"login_host"`
	ExpiresIn       int    `json:"expires_in"`
	IntervalSeconds int    `json:"interval_seconds"`
}

// TraeCNOAuthAccount 是授权完成后可用于建号的凭据。
type TraeCNOAuthAccount struct {
	RefreshToken     string    `json:"refresh_token"`
	AccessToken      string    `json:"access_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	UserID           string    `json:"user_id"`
	Email            string    `json:"email"`
	PlanType         string    `json:"plan_type"`
	LoginHost        string    `json:"login_host"`
	LoginRegion      string    `json:"login_region"`
	UserTag          string    `json:"user_tag,omitempty"`
	LoginTraceID     string    `json:"login_trace_id,omitempty"`
	Warning          string    `json:"warning,omitempty"`
	// Device 是这次授权为该账号绑定的设备码（建号时落库，之后一直用它出站）。
	Device TraeCNDeviceIdentity `json:"device,omitempty"`
}

// TraeCNOAuthStatus 是轮询结果。State 取值 pending / processing / ready / error / expired。
type TraeCNOAuthStatus struct {
	LoginID         string              `json:"login_id"`
	State           string              `json:"state"`
	VerificationURI string              `json:"verification_uri,omitempty"`
	CallbackURL     string              `json:"callback_url,omitempty"`
	ExpiresIn       int                 `json:"expires_in"`
	IntervalSeconds int                 `json:"interval_seconds"`
	Message         string              `json:"message,omitempty"`
	Account         *TraeCNOAuthAccount `json:"account,omitempty"`
}

type traeCNOAuthSession struct {
	mu              sync.Mutex
	loginID         string
	loginTraceID    string
	loginHost       string
	callbackURL     string
	verificationURI string
	codeVerifier    string
	codeChallenge   string
	machineID       string
	deviceID        string
	deviceIdentity  TraeCNDeviceIdentity
	proxyURL        string
	expiresAt       time.Time
	state           string
	message         string
	account         *TraeCNOAuthAccount
}

var (
	traeCNOAuthSessionsMu sync.Mutex
	traeCNOAuthSessions   = map[string]*traeCNOAuthSession{}
)

// TraeCNOAuthCallbackURL 把管理员填写的回调基地址规范化成网关自己的回调地址。
// 只接受 http/https；不带路径时补默认路径。
func TraeCNOAuthCallbackURL(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", fmt.Errorf("回调地址不能为空")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("回调地址无效: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("回调地址必须是 http/https")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("回调地址缺少主机名")
	}
	if strings.TrimSpace(parsed.Path) == "" || parsed.Path == "/" {
		parsed.Path = TraeCNOAuthCallbackPath
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// generateTraeCNOAuthPKCE 生成 S256 PKCE 对。
func generateTraeCNOAuthPKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// traeCNOAuthDevicePublicKey 生成一次性的 P-256 公钥（PEM/SPKI）。授权页只把它写进
// DeviceInfo，私钥不参与后续推理，因此不持久化。
func traeCNOAuthDevicePublicKey() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// TraeCNOAuthDeviceInfo 构造授权码兑换所需的 DeviceInfo。设备码来自本次授权会话
// 新生成的、只属于这个账号的那一份（Trae 有风控，设备码不能多账号共用）。
func TraeCNOAuthDeviceInfo(identity TraeCNDeviceIdentity, publicKey string) map[string]any {
	profile := traeCNFinalizeDeviceProfile(traeCNApplyDeviceIdentity(traeCNBaseDeviceProfile(), identity))
	return map[string]any{
		"DeviceID":        profile.DeviceID,
		"MachineID":       profile.MachineID,
		"PlatformCode":    "IDE_PC",
		"DeviceType":      "PC",
		"DeviceName":      "Trae CN",
		"DeviceModel":     profile.DeviceBrand,
		"ClientVersion":   TraeCNOAuthAppVersion,
		"DevicePublicKey": publicKey,
		"DeviceBrand":     profile.DeviceBrand,
		"DeviceCPU":       profile.DeviceCPU,
		"OSInfo":          profile.DeviceType,
		"OSVersion":       profile.OSVersion,
	}
}

// TraeCNOAuthAuthorizationURL 拼装用户在浏览器里打开的授权链接。
func TraeCNOAuthAuthorizationURL(loginHost, loginTraceID, callbackURL, codeChallenge, machineID, deviceID string) (string, error) {
	host := strings.TrimSpace(loginHost)
	if host == "" {
		host = "https://www.trae.cn"
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "https://" + host
	}
	parsed, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("登录域无效: %w", err)
	}
	parsed.Path = TraeCNOAuthAuthorizationPath
	parsed.RawQuery = ""
	parsed.Fragment = ""

	query := url.Values{}
	query.Set("login_version", "1")
	query.Set("auth_from", "trae")
	query.Set("login_channel", "native_ide")
	query.Set("plugin_version", TraeCNOAuthPluginVersion)
	query.Set("auth_type", "local")
	query.Set("client_id", TraeCNOAuthClientID)
	query.Set("redirect", "0")
	query.Set("login_trace_id", loginTraceID)
	// auth_callback_url 不转义：Trae 授权页按原样使用（客户端就是这么发的）。
	parsed.RawQuery = query.Encode() +
		"&auth_callback_url=" + callbackURL +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=" + url.QueryEscape("Mac") +
		"&x_device_type=" + url.QueryEscape("mac") +
		"&x_os_version=" + url.QueryEscape("macOS") +
		"&x_env=" +
		"&x_app_version=" + url.QueryEscape(TraeCNOAuthAppVersion) +
		"&x_app_type=" + url.QueryEscape(TraeCNOAuthAppType) +
		"&code_challenge=" + url.QueryEscape(codeChallenge) +
		"&code_challenge_method=S256"
	return parsed.String(), nil
}

// RequestTraeCNLoginHost 调 GetLoginGuidance 取登录域。全部失败时回落到 www.trae.cn，
// 与客户端一致：引导接口只是加速，真正的授权域是固定的。
func RequestTraeCNLoginHost(ctx context.Context, loginTraceID, proxyURL string) (string, string) {
	body, _ := json.Marshal(map[string]string{
		"loginTraceID":   loginTraceID,
		"login_trace_id": loginTraceID,
	})
	var failures []string
	for _, host := range traeCNOAuthResolvedGuidanceHosts() {
		target := host + TraeCNOAuthGuidancePath
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
		if err != nil {
			failures = append(failures, target+": "+err.Error())
			continue
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "Trae/1.0.0 codex2api")
		raw, err := traeCNOAuthDo(request, proxyURL)
		if err != nil {
			failures = append(failures, target+": "+err.Error())
			continue
		}
		if loginHost := traeCNOAuthPickString(raw, "Result.LoginHost", "Result.loginHost", "Result.LoginURL", "Result.loginUrl", "result.loginHost", "data.loginHost", "LoginHost", "loginHost"); loginHost != "" {
			return loginHost, ""
		}
		failures = append(failures, target+": 响应缺少 LoginHost")
	}
	return "https://www.trae.cn", strings.Join(failures, " | ")
}

// StartTraeCNOAuth 创建一次授权会话。
func StartTraeCNOAuth(ctx context.Context, callbackURL, proxyURL string) (*TraeCNOAuthStart, error) {
	normalizedCallback, err := TraeCNOAuthCallbackURL(callbackURL)
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := generateTraeCNOAuthPKCE()
	if err != nil {
		return nil, fmt.Errorf("生成 PKCE 失败: %w", err)
	}
	loginID := uuid.NewString()
	traceID := uuid.NewString()
	// 授权链接和 DeviceInfo 都带上属于这个账号的设备码：登录一开始就把账号和设备绑定，
	// 之后推理/签到继续用同一份。
	identity := NewTraeCNDeviceIdentity()
	profile := traeCNFinalizeDeviceProfile(traeCNApplyDeviceIdentity(traeCNBaseDeviceProfile(), identity))

	loginHost, guidanceErr := RequestTraeCNLoginHost(ctx, traceID, proxyURL)
	verificationURI, err := TraeCNOAuthAuthorizationURL(loginHost, traceID, normalizedCallback, challenge, profile.MachineID, profile.DeviceID)
	if err != nil {
		return nil, err
	}

	session := &traeCNOAuthSession{
		loginID:         loginID,
		loginTraceID:    traceID,
		loginHost:       loginHost,
		callbackURL:     normalizedCallback,
		verificationURI: verificationURI,
		codeVerifier:    verifier,
		codeChallenge:   challenge,
		machineID:       profile.MachineID,
		deviceID:        profile.DeviceID,
		deviceIdentity:  identity,
		proxyURL:        strings.TrimSpace(proxyURL),
		expiresAt:       time.Now().Add(TraeCNOAuthSessionTTL),
		state:           "pending",
		message:         guidanceErr,
	}
	traeCNOAuthSessionsMu.Lock()
	purgeTraeCNOAuthSessionsLocked(time.Now())
	traeCNOAuthSessions[loginID] = session
	traeCNOAuthSessions[traceID] = session
	traeCNOAuthSessionsMu.Unlock()

	return &TraeCNOAuthStart{
		LoginID:         loginID,
		LoginTraceID:    traceID,
		VerificationURI: verificationURI,
		CallbackURL:     normalizedCallback,
		LoginHost:       loginHost,
		ExpiresIn:       int(TraeCNOAuthSessionTTL / time.Second),
		IntervalSeconds: 2,
	}, nil
}

// purgeTraeCNOAuthSessionsLocked 清理过期会话。login_id 与 login_trace_id 都指向同一个
// 会话对象，所以按会话过期时间删掉两个索引键。
func purgeTraeCNOAuthSessionsLocked(now time.Time) {
	for key, session := range traeCNOAuthSessions {
		if session == nil || now.After(session.expiresAt) {
			delete(traeCNOAuthSessions, key)
		}
	}
}

// FindTraeCNOAuthSession 按 login_id 或 login_trace_id 找到会话（回调只带 trace）。
func FindTraeCNOAuthSession(key string) *traeCNOAuthSession {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	traeCNOAuthSessionsMu.Lock()
	defer traeCNOAuthSessionsMu.Unlock()
	session := traeCNOAuthSessions[key]
	if session != nil && time.Now().After(session.expiresAt) {
		return nil
	}
	return session
}

// TraeCNOAuthStatusFor 返回轮询结果。
func TraeCNOAuthStatusFor(loginID string) (*TraeCNOAuthStatus, bool) {
	session := FindTraeCNOAuthSession(loginID)
	if session == nil {
		return nil, false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	status := &TraeCNOAuthStatus{
		LoginID:         session.loginID,
		State:           session.state,
		VerificationURI: session.verificationURI,
		CallbackURL:     session.callbackURL,
		ExpiresIn:       int(time.Until(session.expiresAt) / time.Second),
		IntervalSeconds: 2,
		Message:         session.message,
		Account:         session.account,
	}
	if status.ExpiresIn < 0 {
		status.ExpiresIn = 0
	}
	if status.State == "pending" && time.Now().After(session.expiresAt) {
		status.State = "expired"
	}
	return status, true
}

// CancelTraeCNOAuth 丢弃一次授权会话。
func CancelTraeCNOAuth(loginID string) {
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return
	}
	traeCNOAuthSessionsMu.Lock()
	defer traeCNOAuthSessionsMu.Unlock()
	if session := traeCNOAuthSessions[loginID]; session != nil {
		delete(traeCNOAuthSessions, session.loginID)
		delete(traeCNOAuthSessions, session.loginTraceID)
	}
}

// traeCNOAuthCallbackPayload 是回调查询串里我们关心的字段。
type traeCNOAuthCallbackPayload struct {
	authCode      string
	refreshToken  string
	cloudideToken string
	userTag       string
	loginHost     string
	loginRegion   string
	loginTraceID  string
	userInfo      map[string]any
}

// ParseTraeCNOAuthCallback 从回调链接（完整 URL 或 query 串）里解析授权结果。
func ParseTraeCNOAuthCallback(raw string) (*traeCNOAuthCallbackPayload, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("回调内容为空")
	}
	values, err := traeCNOAuthCallbackValues(raw)
	if err != nil {
		return nil, err
	}
	payload := &traeCNOAuthCallbackPayload{
		authCode:      traeCNOAuthPick(values, "authCode", "auth_code", "AuthCode", "authorization_code", "code"),
		refreshToken:  traeCNOAuthPick(values, "refreshToken", "refresh_token", "RefreshToken"),
		cloudideToken: traeCNOAuthPick(values, "x-cloudide-token", "xCloudideToken", "accessToken", "access_token", "token"),
		userTag:       traeCNOAuthPick(values, "userTag", "user_tag", "usertag"),
		loginHost:     traeCNOAuthPick(values, "loginHost", "login_host"),
		loginRegion:   traeCNOAuthPick(values, "loginRegion", "login_region", "region"),
		loginTraceID:  traeCNOAuthPick(values, "loginTraceId", "login_trace_id", "loginTraceID"),
	}
	if payload.authCode == "" {
		if rawInfo := traeCNOAuthPick(values, "authCodeInfo", "auth_code_info", "AuthCodeInfo"); rawInfo != "" {
			var parsed map[string]any
			if json.Unmarshal([]byte(rawInfo), &parsed) == nil {
				payload.authCode = traeCNOAuthMapString(parsed, "AuthCode", "authCode", "auth_code", "code")
			}
		}
	}
	if rawUser := traeCNOAuthPick(values, "userInfo", "user_info"); rawUser != "" {
		var parsed map[string]any
		if json.Unmarshal([]byte(rawUser), &parsed) == nil {
			payload.userInfo = parsed
		}
	}
	if payload.authCode == "" && payload.refreshToken == "" && payload.cloudideToken == "" {
		return nil, fmt.Errorf("回调链接里没有 authCode / refreshToken")
	}
	return payload, nil
}

func traeCNOAuthCallbackValues(raw string) (url.Values, error) {
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("回调链接无效: %w", err)
		}
		if parsed.RawQuery == "" && parsed.Fragment == "" {
			return nil, fmt.Errorf("回调链接里没有参数")
		}
		values := parsed.Query()
		if parsed.Fragment != "" {
			if fragment, err := url.ParseQuery(parsed.Fragment); err == nil {
				for key, list := range fragment {
					for _, value := range list {
						values.Add(key, value)
					}
				}
			}
		}
		return values, nil
	}
	values, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
	if err != nil {
		return nil, fmt.Errorf("回调参数无效: %w", err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("回调参数为空")
	}
	return values, nil
}

func traeCNOAuthPick(values url.Values, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(values.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func traeCNOAuthMapString(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			if text := traeTokenString(value); strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	for _, nested := range []string{"Result", "result", "data"} {
		if child, ok := root[nested].(map[string]any); ok {
			if text := traeCNOAuthMapString(child, keys...); text != "" {
				return text
			}
		}
	}
	return ""
}

// CompleteTraeCNOAuth 用回调内容完成一次授权（幂等：重复提交返回同一账号）。
func CompleteTraeCNOAuth(ctx context.Context, loginID, rawCallback string) (*TraeCNOAuthAccount, error) {
	session := FindTraeCNOAuthSession(loginID)
	if session == nil {
		return nil, fmt.Errorf("授权会话不存在或已过期，请重新发起")
	}
	session.mu.Lock()
	if session.account != nil {
		account := *session.account
		session.mu.Unlock()
		return &account, nil
	}
	if session.state == "processing" {
		session.mu.Unlock()
		return nil, fmt.Errorf("授权正在处理中")
	}
	session.state = "processing"
	verifier := session.codeVerifier
	proxyURL := session.proxyURL
	machineID := session.machineID
	deviceID := session.deviceID
	loginHost := session.loginHost
	session.mu.Unlock()

	payload, err := ParseTraeCNOAuthCallback(rawCallback)
	if err != nil {
		session.mu.Lock()
		session.state = "error"
		session.message = err.Error()
		session.mu.Unlock()
		return nil, err
	}
	deviceIdentity := session.deviceIdentity
	account, err := traeCNOAuthExchange(ctx, payload, verifier, machineID, deviceID, loginHost, proxyURL)
	if err == nil && account != nil {
		// 授权成功即把这个账号与设备码绑定（建号时落库）。
		account.Device = deviceIdentity
	}
	if err != nil {
		session.mu.Lock()
		session.state = "error"
		session.message = err.Error()
		session.mu.Unlock()
		return nil, err
	}
	account.LoginTraceID = payload.loginTraceID
	if payload.loginTraceID != "" && session.loginTraceID != "" && payload.loginTraceID != session.loginTraceID {
		// trace 不匹配只是提示（可能命中缓存会话），与客户端一致：继续处理。
		account.Warning = "回调 login_trace_id 与会话不一致，已按当前会话完成授权"
	}
	session.mu.Lock()
	session.state = "ready"
	session.message = ""
	session.account = account
	session.mu.Unlock()
	return account, nil
}

// traeCNOAuthExchange 完成授权码/RT 到长期凭据的兑换，并补齐账号信息。
func traeCNOAuthExchange(ctx context.Context, payload *traeCNOAuthCallbackPayload, codeVerifier, machineID, deviceID, loginHost, proxyURL string) (*TraeCNOAuthAccount, error) {
	account := &TraeCNOAuthAccount{
		LoginHost:   loginHost,
		LoginRegion: normalizeTraeCNLoginRegion(payload.loginRegion, loginHost),
		UserTag:     payload.userTag,
	}
	if payload.authCode != "" {
		token, err := ExchangeTraeCNOAuthCode(ctx, payload.authCode, codeVerifier, machineID, deviceID, proxyURL)
		if err != nil {
			return nil, err
		}
		applyTraeCNToken(account, token)
	} else {
		refreshToken := payload.refreshToken
		token, err := ExchangeTraeCNRefreshToken(ctx, refreshToken, "", proxyURL)
		if err != nil {
			return nil, fmt.Errorf("RT 兑换失败: %w", err)
		}
		applyTraeCNToken(account, token)
		if account.RefreshToken == "" {
			account.RefreshToken = refreshToken
		}
	}
	if account.AccessToken == "" {
		return nil, fmt.Errorf("授权成功但上游没有返回 access token")
	}
	info, err := RequestTraeCNUserInfo(ctx, account.AccessToken, payload.cloudideToken, loginHost, proxyURL)
	if err == nil && info != nil {
		if account.Email == "" {
			account.Email = info.Email
		}
		if account.UserID == "" {
			account.UserID = info.UserID
		}
	}
	if account.UserID == "" {
		account.UserID = jwtUserID(account.AccessToken)
	}
	if account.Email == "" && payload.userInfo != nil {
		account.Email = traeCNOAuthMapString(payload.userInfo, "NonPlainTextEmail", "Email", "email")
	}
	return account, nil
}

func applyTraeCNToken(account *TraeCNOAuthAccount, token TraeCNToken) {
	account.AccessToken = token.AccessToken
	account.RefreshToken = token.RefreshToken
	account.ExpiresAt = token.ExpiresAt
	account.RefreshExpiresAt = token.RefreshExpiresAt
	if token.UserID != "" {
		account.UserID = token.UserID
	}
}

// ExchangeTraeCNOAuthCode 用授权码 + PKCE verifier 换长期凭据。
func ExchangeTraeCNOAuthCode(ctx context.Context, authCode, codeVerifier, machineID, deviceID, proxyURL string) (TraeCNToken, error) {
	authCode = strings.TrimSpace(authCode)
	if authCode == "" {
		return TraeCNToken{}, fmt.Errorf("authCode 不能为空")
	}
	if strings.TrimSpace(codeVerifier) == "" {
		return TraeCNToken{}, fmt.Errorf("授权会话缺少 code verifier，请重新发起登录")
	}
	publicKey, err := traeCNOAuthDevicePublicKey()
	if err != nil {
		return TraeCNToken{}, fmt.Errorf("生成设备公钥失败: %w", err)
	}
	identity := TraeCNDeviceIdentity{MachineID: strings.TrimSpace(machineID), DeviceID: strings.TrimSpace(deviceID)}
	if identity.Empty() {
		// 没有会话设备码时按授权码派生一份，保证请求里始终带一致的设备码。
		identity = DeriveTraeCNDeviceIdentity("oauth:" + authCode)
	}
	deviceInfo := TraeCNOAuthDeviceInfo(identity, publicKey)
	body, err := json.Marshal(map[string]any{
		"ClientID":     TraeCNOAuthClientID,
		"AuthCode":     authCode,
		"CodeVerifier": codeVerifier,
		"DeviceInfo":   deviceInfo,
		"IDEVersion":   TraeCNOAuthAppVersion,
	})
	if err != nil {
		return TraeCNToken{}, err
	}
	target := traeCNOAuthResolvedAuthHost() + TraeCNOAuthAuthCodeExchangePth
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return TraeCNToken{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-cloudide-token", "")
	client := traeCNOAuthClient(proxyURL)
	resp, err := client.Do(request)
	if err != nil {
		return TraeCNToken{}, fmt.Errorf("授权码兑换请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return TraeCNToken{}, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TraeCNToken{}, fmt.Errorf("授权码兑换失败: HTTP %d %s", resp.StatusCode, traeCNOAuthTruncate(raw))
	}
	return normalizeTraeTokenResponse(raw)
}

// TraeCNOAuthUserInfo 是从 GetUserInfo 里取到的账号信息。
type TraeCNOAuthUserInfo struct {
	Email  string
	UserID string
}

// RequestTraeCNUserInfo 取账号信息；失败不影响建号（调用方自行降级）。
func RequestTraeCNUserInfo(ctx context.Context, accessToken, cloudideToken, loginHost, proxyURL string) (*TraeCNOAuthUserInfo, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("缺少 access token")
	}
	host := strings.TrimSpace(loginHost)
	if host == "" {
		host = traeCNOAuthResolvedAuthHost()
	}
	target := strings.TrimRight(host, "/") + TraeCNOAuthUserInfoPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Cloud-IDE-JWT "+accessToken)
	if token := strings.TrimSpace(cloudideToken); token != "" {
		request.Header.Set("x-cloudide-token", token)
	}
	resp, err := traeCNOAuthClient(proxyURL).Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GetUserInfo HTTP %d %s", resp.StatusCode, traeCNOAuthTruncate(raw))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	info := &TraeCNOAuthUserInfo{
		Email: traeCNOAuthMapString(decoded,
			"NonPlainTextEmail", "Email", "email"),
		UserID: traeCNOAuthMapString(decoded,
			"UserID", "userId", "user_id", "UID", "uid"),
	}
	return info, nil
}

func normalizeTraeCNLoginRegion(region, loginHost string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	switch region {
	case "cn", "sg", "us":
		return region
	}
	host := strings.ToLower(loginHost)
	switch {
	case strings.Contains(host, ".cn"):
		return "cn"
	case strings.Contains(host, ".us"):
		return "us"
	case host == "":
		return ""
	default:
		return "sg"
	}
}

func traeCNOAuthClient(proxyURL string) *http.Client {
	client := &http.Client{Timeout: TraeCNOAuthTimeout}
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if err := ConfigureTransportProxy(transport, proxyURL, nil); err == nil {
			client.Transport = transport
		}
	}
	return client
}

func traeCNOAuthDo(request *http.Request, proxyURL string) (map[string]any, error) {
	resp, err := traeCNOAuthClient(proxyURL).Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, traeCNOAuthTruncate(raw))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return decoded, nil
}

func traeCNOAuthPickString(root map[string]any, paths ...string) string {
	for _, path := range paths {
		parts := strings.Split(path, ".")
		var current any = root
		ok := true
		for _, part := range parts {
			container, isMap := current.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			current, ok = container[part]
			if !ok {
				break
			}
		}
		if !ok || current == nil {
			continue
		}
		if text := strings.TrimSpace(traeTokenString(current)); text != "" {
			return text
		}
	}
	return ""
}

func traeCNOAuthTruncate(raw []byte) string {
	message := strings.TrimSpace(string(raw))
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}
