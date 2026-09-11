package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// UpstreamTraeCN marks Trae China edition accounts. The account credential
// shape is refresh_token plus a short-lived access_token issued by Trae's
// ExchangeToken endpoint.
const UpstreamTraeCN = "traecn"

// Trae CN model credentials are kept separate from the generic `models`
// field. `traecn_upstream_models` is the last catalog returned by the
// provider; `traecn_model_allowlist` is an optional account-level narrowing
// configured by an administrator. The generic `models` projection continues
// to contain the effective routable intersection for older callers.
const (
	TraeCNUpstreamModelsCredentialKey = "traecn_upstream_models"
	TraeCNModelAllowlistCredentialKey = "traecn_model_allowlist"
	// TraeCNModelAllowlistSetCredentialKey distinguishes an administrator's
	// explicit empty allowlist (follow the whole upstream catalog) from a
	// legacy row that has never been migrated to the dedicated fields.
	TraeCNModelAllowlistSetCredentialKey = "traecn_model_allowlist_set"
	TraeCNModelsSyncedAtCredentialKey    = "traecn_models_synced_at"
)

const (
	TraeCNDefaultHost = "https://trae-api-cn.mchost.guru"
	// TraeCNAuthHost is the current Trae CN account/authentication domain.
	// The agent/chat service still uses TraeCNDefaultHost, but its historical
	// ExchangeToken route now returns a TLB 404 there.
	TraeCNAuthHost      = "https://api.trae.cn"
	TraeCNExchangePath  = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	TraeCNChatPath      = "/api/agent/v3/llm_utils_chat"
	TraeCNOAuthClientID = "ono9krqynydwx5"
	TraeCNAppID         = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	TraeCNDefaultIDE    = "3.3.99"
	// TraeCNDefaultIDECode 是 Trae CN 3.3.99 桌面端真实发布的版本码（抓包实测
	// x-ide-version-code 就是这个值）。Trae 按它灰度下发模型目录：同一账号同一
	// 令牌下 20260212 只给 85 个 config（没有 glm-5.3-flash），20260901 起给 107
	// 个。它是客户端的发布标识、必须保持真实且恒定——不能按当天日期伪造，否则
	// 每天变化的版本码本身就是异常指纹。客户端升级后用 TRAECN_IDE_VERSION_CODE
	// 覆盖，或更新这里的常量。
	TraeCNDefaultIDECode   = "20260901"
	TraeCNDefaultUserAgent = "node-fetch/1.0 (+https://github.com/bitinn/node-fetch)"
	TraeCNExchangeTimeout  = 30 * time.Second
	TraeCNAccessTokenGrace = 5 * time.Minute
	// TraeCNRefreshCriticalTimeout bounds the RT-consumption critical section
	// when the caller does not already hold a distributed OAuth lease.  The
	// context deliberately outlives a cancelled HTTP request: ExchangeToken may
	// rotate the RT before the response is fully read, and cancelling the DB
	// publication at that point would strand the replacement credential.
	TraeCNRefreshCriticalTimeout = TraeCNExchangeTimeout + 15*time.Second
)

// NormalizeTraeCNHost validates and normalizes a Trae CN API host. Only an
// origin is accepted so an imported credential cannot smuggle a path/query
// into the fixed Trae endpoint.
func NormalizeTraeCNHost(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return TraeCNDefaultHost, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("traecn host must be an http(s) origin")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/"), nil
}

func (a *Account) isTraeCNAPILocked() bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamTraeCN) {
		return false
	}
	return strings.TrimSpace(a.AccessToken) != "" || strings.TrimSpace(a.RefreshToken) != ""
}

// IsTraeCNAPI reports whether an account belongs to the Trae CN channel.
func (a *Account) IsTraeCNAPI() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isTraeCNAPILocked()
}

// TraeCNCredentials returns the configured host and current access token.
func (a *Account) TraeCNCredentials() (host, accessToken string) {
	if a == nil {
		return "", ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	host = strings.TrimRight(strings.TrimSpace(a.TraeCNHost), "/")
	if host == "" {
		host = strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	}
	if normalized, err := NormalizeTraeCNHost(host); err == nil {
		host = normalized
	} else {
		host = TraeCNDefaultHost
	}
	return host, strings.TrimSpace(a.AccessToken)
}

// TraeCNRefreshToken returns the RT without exposing other account fields.
func (a *Account) TraeCNRefreshToken() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isTraeCNAPILocked() {
		return ""
	}
	return strings.TrimSpace(a.RefreshToken)
}

// TraeCNUser returns the optional provider user id used in request headers.
func (a *Account) TraeCNUser() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.TraeCNUserID)
}

// TraeCNModels returns the optional per-account model whitelist. An empty list
// means that the account has not narrowed the provider catalog; callers that
// need the effective routable catalog should use TraeCNEffectiveModels.
func (a *Account) TraeCNModels() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isTraeCNAPILocked() {
		return nil
	}
	return cloneStringSlice(a.Models)
}

// TraeCNUpstreamModelIDs returns the last provider model catalog fetched for
// this account. It is intentionally distinct from the administrator's
// allowlist so a refresh can update provider capabilities without erasing a
// local narrowing rule.
func (a *Account) TraeCNUpstreamModelIDs() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isTraeCNAPILocked() {
		return nil
	}
	return cloneStringSlice(a.TraeCNUpstreamModelCatalog)
}

// TraeCNConfiguredModelAllowlist returns the optional administrator override.
func (a *Account) TraeCNConfiguredModelAllowlist() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isTraeCNAPILocked() {
		return nil
	}
	if a.TraeCNModelAllowlistSet || len(a.TraeCNModelAllowlist) > 0 {
		return cloneStringSlice(a.TraeCNModelAllowlist)
	}
	// Rows written by versions before the separate catalog fields used the
	// generic Models value for the optional Trae narrowing list. Keep those rows
	// compatible when no fetched catalog is present yet.
	if len(a.TraeCNUpstreamModelCatalog) == 0 {
		return traeCNLegacyModelAllowlist(a.Models)
	}
	return nil
}

// TraeCNModelCatalogSyncedAt returns the last successful upstream catalog
// refresh timestamp.
func (a *Account) TraeCNModelCatalogSyncedAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.TraeCNModelCatalogSyncedAtValue
}

func traeCNModelIntersection(catalog, allowlist []string) []string {
	catalog = normalizeModelList(catalog)
	if len(allowlist) == 0 {
		return catalog
	}
	allowlist = normalizeModelList(allowlist)
	result := make([]string, 0, len(catalog))
	for _, model := range catalog {
		for _, allowed := range allowlist {
			if TraeCNModelsEquivalent(model, allowed) {
				result = append(result, model)
				break
			}
		}
	}
	return result
}

// traeCNModelListsEqual compares normalized model lists while treating nil
// and empty slices as the same value. Catalogs are persisted in normalized
// order, but normalizing here also makes cross-instance reloads insensitive to
// casing/order differences produced by older rows.
func traeCNModelListsEqual(left, right []string) bool {
	left = normalizeModelList(left)
	right = normalizeModelList(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !strings.EqualFold(left[index], right[index]) {
			return false
		}
	}
	return true
}

// traeCNLegacyModelAllowlist recovers the optional allowlist used by rows
// written before the dedicated Trae catalog fields existed. An untouched row
// may have had the built-in catalog materialized into `models`; that projection
// is not an administrator restriction and must not freeze future upstream
// models after the first cross-instance reload.
func traeCNLegacyModelAllowlist(models []string) []string {
	models = normalizeModelList(models)
	if len(models) == 0 || traeCNModelListsEqual(models, TraeCNDefaultModelIDs()) {
		return nil
	}
	return models
}

// TraeCNIntersectModelIDs applies an optional Trae account allowlist to a
// fetched provider catalog. It is exported for admin/API layers so they use
// the same public-alias and wire-name matching rules as runtime routing.
func TraeCNIntersectModelIDs(catalog, allowlist []string) []string {
	return traeCNModelIntersection(catalog, allowlist)
}

// traeCNIsCanonicalPublicModelID reports whether a model name is a catalog ID
// as advertised by the provider (as opposed to a wire/config name). Without a
// built-in alias table the two coincide; the check is kept for the allowlist
// and intersection paths that still need to recognize catalog spellings.
func traeCNIsCanonicalPublicModelID(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, publicID := range TraeCNDefaultModelIDs() {
		if strings.EqualFold(publicID, model) {
			return true
		}
	}
	return false
}

// TraeCNModelsEquivalent compares two model identifiers。内置别名表已删除，因此不再
// 按「上游 wire 名」互相换算，只允许大小写与分隔符差异：provider 目录写
// DeepSeek-V4-Pro / Doubao_1_6，网关目录写 deepseek-v4-pro / doubao-1-6，它们指向
// 同一个模型，必须仍然匹配；deepseek-v3 与 deepseek-v4-pro 这类不同模型不再互相
// 命中。真正的改名交给管理员的 TRAECN 模型映射。
func TraeCNModelsEquivalent(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if strings.EqualFold(left, right) {
		return true
	}
	leftKey, rightKey := traeCNModelKey(left), traeCNModelKey(right)
	return leftKey != "" && leftKey == rightKey
}

// traeCNModelKey 归一化模型名：只保留字母数字，用于跨命名风格比较。
func traeCNModelKey(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// traeCNEffectiveModelsLocked computes the routable logical catalog. The
// caller must hold a.mu for reading or writing.
func traeCNEffectiveModelsLocked(a *Account) []string {
	if a == nil || !a.isTraeCNAPILocked() {
		return nil
	}
	catalog := cloneStringSlice(a.TraeCNUpstreamModelCatalog)
	if len(catalog) == 0 {
		catalog = TraeCNDefaultModelIDs()
	}
	allowlist := cloneStringSlice(a.TraeCNModelAllowlist)
	if !a.TraeCNModelAllowlistSet && len(allowlist) == 0 && len(a.TraeCNUpstreamModelCatalog) == 0 {
		// Backward compatibility for pre-catalog rows and in-memory fixtures.
		allowlist = cloneStringSlice(a.Models)
	}
	return traeCNModelIntersection(catalog, allowlist)
}

// TraeCNEffectiveModels returns the logical model catalog used for dispatch.
// It is the provider catalog (fetched from /v1/models when available, with a
// built-in compatibility catalog as a cold-start fallback) narrowed by the
// optional account allowlist.
func (a *Account) TraeCNEffectiveModels() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return traeCNEffectiveModelsLocked(a)
}

// TraeCNModelsForAllowlist calculates the effective catalog with a proposed
// administrator allowlist without mutating the account. It is used by admin
// handlers when persisting the legacy `models` projection alongside the new
// Trae-specific catalog fields.
func (a *Account) TraeCNModelsForAllowlist(allowlist []string) []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isTraeCNAPILocked() {
		return nil
	}
	catalog := cloneStringSlice(a.TraeCNUpstreamModelCatalog)
	if len(catalog) == 0 {
		catalog = TraeCNDefaultModelIDs()
	}
	return traeCNModelIntersection(catalog, allowlist)
}

// EnsureTraeCNAccessToken refreshes an account lazily when its AT is missing
// or close to expiry. It updates only in-memory state; Store refresh paths add
// the durable credentials write after this succeeds.
func (a *Account) EnsureTraeCNAccessToken(ctx context.Context, proxyURL string, forceRefresh bool) error {
	if a == nil {
		return fmt.Errorf("traecn account is unavailable")
	}
	a.traeRefreshMu.Lock()
	defer a.traeRefreshMu.Unlock()
	a.mu.RLock()
	accessToken := strings.TrimSpace(a.AccessToken)
	expiresAt := a.ExpiresAt
	refreshToken := strings.TrimSpace(a.RefreshToken)
	host := strings.TrimSpace(a.TraeCNHost)
	dbID := a.DBID
	if host == "" {
		host = strings.TrimSpace(a.BaseURL)
	}
	a.mu.RUnlock()
	if !forceRefresh && accessToken != "" && !expiresAt.IsZero() && time.Until(expiresAt) > TraeCNAccessTokenGrace {
		return nil
	}
	if refreshToken == "" {
		if accessToken != "" && (expiresAt.IsZero() || time.Now().Before(expiresAt)) {
			return nil
		}
		if accessToken != "" {
			return fmt.Errorf("traecn access_token is expired and refresh_token is empty")
		}
		return fmt.Errorf("traecn refresh_token is empty")
	}
	resinAccountID := ""
	if dbID > 0 {
		resinAccountID = strconv.FormatInt(dbID, 10)
	}
	token, err := ExchangeTraeCNRefreshToken(ctx, refreshToken, host, proxyURL, resinAccountID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		a.RefreshToken = token.RefreshToken
	}
	a.ExpiresAt = token.ExpiresAt
	if token.UserID != "" {
		a.TraeCNUserID = token.UserID
		a.AccountID = token.UserID
	}
	a.mu.Unlock()
	return nil
}

func (a *Account) TraeCNSupportsModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	model = TraeCNRequestModel(model)
	models := a.TraeCNEffectiveModels()
	for _, candidate := range models {
		if TraeCNModelsEquivalent(candidate, model) {
			return true
		}
	}
	return false
}

// ApplyTraeCNConfig updates the in-memory projection after the admin settings
// endpoint persists an account's host, model allowlist, or proxy.
func (s *Store) ApplyTraeCNConfig(dbID int64, host string, models []string, proxyURL string) bool {
	if s == nil {
		return false
	}
	account := s.FindByID(dbID)
	if account == nil {
		return false
	}
	normalizedHost, err := NormalizeTraeCNHost(host)
	if err != nil {
		return false
	}
	account.mu.Lock()
	account.TraeCNHost = normalizedHost
	account.TraeCNModelAllowlist = normalizeModelList(models)
	account.TraeCNModelAllowlistSet = true
	// This is an explicit administrator update. Do not reuse the legacy
	// `Models` projection as an implicit allowlist when the user clears it.
	// Legacy fallback is only needed while loading an untouched pre-catalog row.
	catalog := cloneStringSlice(account.TraeCNUpstreamModelCatalog)
	if len(catalog) == 0 {
		catalog = TraeCNDefaultModelIDs()
	}
	account.Models = traeCNModelIntersection(catalog, account.TraeCNModelAllowlist)
	account.ProxyURL = strings.TrimSpace(proxyURL)
	account.mu.Unlock()
	return true
}

// ApplyTraeCNUpstreamModels publishes a freshly fetched provider catalog to
// the in-memory account. The effective `Models` projection is recomputed so
// routing and /v1/models observe the same catalog immediately.
func (s *Store) ApplyTraeCNUpstreamModels(dbID int64, models []string, syncedAt time.Time) bool {
	return s.applyTraeCNUpstreamModels(dbID, models, syncedAt, nil, false)
}

// ApplyTraeCNUpstreamModelsWithAllowlist publishes a provider catalog and an
// explicit account allowlist atomically. The explicit setter is used during a
// sync to migrate legacy rows whose generic `models` field represented the
// allowlist before the Trae-specific credential fields were introduced.
func (s *Store) ApplyTraeCNUpstreamModelsWithAllowlist(dbID int64, models []string, syncedAt time.Time, allowlist []string) bool {
	return s.applyTraeCNUpstreamModels(dbID, models, syncedAt, allowlist, true)
}

func (s *Store) applyTraeCNUpstreamModels(dbID int64, models []string, syncedAt time.Time, allowlist []string, setAllowlist bool) bool {
	if s == nil {
		return false
	}
	account := s.FindByID(dbID)
	if account == nil {
		return false
	}
	normalized := normalizeModelList(models)
	if len(normalized) == 0 {
		return false
	}
	account.mu.Lock()
	if !account.isTraeCNAPILocked() {
		account.mu.Unlock()
		return false
	}
	account.TraeCNUpstreamModelCatalog = normalized
	if setAllowlist {
		account.TraeCNModelAllowlist = normalizeModelList(allowlist)
		account.TraeCNModelAllowlistSet = true
	}
	account.TraeCNModelCatalogSyncedAtValue = syncedAt.UTC()
	account.Models = traeCNEffectiveModelsLocked(account)
	account.mu.Unlock()
	return true
}

// TraeCNToken is the normalized response returned by ExchangeToken.
type TraeCNToken struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        time.Time
	RefreshExpiresAt time.Time
	TokenReleaseAt   time.Time
	UserID           string
}

func parseTraeTime(value any) time.Time {
	normalizeUnix := func(seconds float64) time.Time {
		if seconds <= 0 {
			return time.Time{}
		}
		// ExchangeToken deployments have returned seconds, milliseconds and
		// (in a few archived desktop builds) microseconds. Normalize all three
		// without turning a millisecond value into a date thousands of years out.
		switch {
		case seconds >= 1e18:
			seconds /= 1e9
		case seconds >= 1e15:
			seconds /= 1e6
		case seconds >= 1e12:
			seconds /= 1e3
		}
		return time.Unix(int64(seconds), 0).UTC()
	}
	switch typed := value.(type) {
	case float64:
		return normalizeUnix(typed)
	case float32:
		return normalizeUnix(float64(typed))
	case int:
		return normalizeUnix(float64(typed))
	case int64:
		return normalizeUnix(float64(typed))
	case int32:
		return normalizeUnix(float64(typed))
	case json.Number:
		if n, err := typed.Float64(); err == nil {
			return normalizeUnix(n)
		}
	case string:
		s := strings.TrimSpace(typed)
		if s == "" {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339, s); err == nil {
			return parsed.UTC()
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return normalizeUnix(n)
		}
	}
	return time.Time{}
}

func decodeJWTPart(part string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(part), "="))
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	decoded, err := decodeJWTPart(parts[1])
	if err != nil {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(decoded, &payload) != nil {
		return nil
	}
	return payload
}

func traeTokenString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []byte:
		return strings.TrimSpace(string(typed))
	case json.Number:
		return strings.TrimSpace(typed.String())
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	default:
		// In particular, nil must stay empty rather than becoming "<nil>".
		return ""
	}
}

// traeResponseMaps walks only the documented response-envelope keys. It
// accepts data/result nesting used by regional gateways without recursively
// searching arbitrary provider objects where an unrelated "token" field could
// otherwise be mistaken for the OAuth access token.
func traeResponseMaps(root map[string]any) []map[string]any {
	if root == nil {
		return nil
	}
	result := []map[string]any{root}
	queue := []map[string]any{root}
	for depth := 0; depth < 4 && len(queue) > 0; depth++ {
		next := make([]map[string]any, 0)
		for _, current := range queue {
			for _, key := range []string{
				"data", "Data", "result", "Result", "token_data", "tokenData", "TokenData",
				"response", "Response", "ResponseMetadata", "Error",
			} {
				if nested, ok := current[key].(map[string]any); ok && nested != nil {
					result = append(result, nested)
					next = append(next, nested)
				}
			}
		}
		queue = next
	}
	return result
}

func traeResponseLookup(roots []map[string]any, keys ...string) any {
	for _, root := range roots {
		for _, key := range keys {
			if value, ok := root[key]; ok && value != nil {
				return value
			}
		}
	}
	return nil
}

func parseTraeExpiresIn(value any) time.Time {
	var seconds float64
	switch typed := value.(type) {
	case float64:
		seconds = typed
	case float32:
		seconds = float64(typed)
	case int:
		seconds = float64(typed)
	case int64:
		seconds = float64(typed)
	case json.Number:
		seconds, _ = typed.Float64()
	case string:
		seconds, _ = strconv.ParseFloat(strings.TrimSpace(typed), 64)
	}
	if seconds <= 0 {
		return time.Time{}
	}
	// Some APIs spell expires_in in milliseconds. Durations above one year are
	// implausible for an AT and are therefore treated as milliseconds.
	if seconds > 365*24*60*60 {
		seconds /= 1000
	}
	return time.Now().UTC().Add(time.Duration(seconds * float64(time.Second)))
}

func normalizeTraeTokenResponse(raw []byte) (TraeCNToken, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return TraeCNToken{}, fmt.Errorf("decode ExchangeToken response: %w", err)
	}
	roots := traeResponseMaps(root)
	lookup := func(keys ...string) any { return traeResponseLookup(roots, keys...) }
	result := TraeCNToken{
		AccessToken:      traeTokenString(lookup("token", "Token", "access_token", "accessToken", "AccessToken")),
		RefreshToken:     traeTokenString(lookup("refreshToken", "RefreshToken", "refresh_token", "Refresh_Token")),
		ExpiresAt:        parseTraeTime(lookup("expiredAt", "ExpiredAt", "expired_at", "expiresAt", "expires_at", "TokenExpireAt", "tokenExpireAt")),
		RefreshExpiresAt: parseTraeTime(lookup("refreshExpiredAt", "RefreshExpiredAt", "refreshExpiredAt", "refresh_expired_at", "RefreshExpireAt", "refreshExpireAt")),
		TokenReleaseAt:   parseTraeTime(lookup("tokenReleaseAt", "TokenReleaseAt", "token_release_at")),
		UserID:           traeTokenString(lookup("userId", "UserId", "UserID", "user_id", "uid", "UID")),
	}
	if result.AccessToken == "" {
		code := traeTokenString(lookup("code", "Code", "error_code", "errorCode", "ErrorCode"))
		message := traeTokenString(lookup("message", "Message", "msg", "Msg", "error_description", "errorDescription", "ErrorDescription", "error", "Error"))
		if message != "" {
			if code != "" && code != "0" {
				return TraeCNToken{}, fmt.Errorf("ExchangeToken response missing token: %s (%s)", message, code)
			}
			return TraeCNToken{}, fmt.Errorf("ExchangeToken response missing token: %s", message)
		}
		return TraeCNToken{}, fmt.Errorf("ExchangeToken response missing token")
	}
	if result.UserID == "" {
		result.UserID = jwtUserID(result.AccessToken)
	}
	if result.ExpiresAt.IsZero() {
		result.ExpiresAt = jwtExpiry(result.AccessToken)
	}
	if result.ExpiresAt.IsZero() {
		result.ExpiresAt = parseTraeExpiresIn(lookup("expiresIn", "expires_in", "expires"))
	}
	if result.ExpiresAt.IsZero() {
		result.ExpiresAt = time.Now().UTC().Add(30 * time.Minute)
	}
	return result, nil
}

func jwtUserID(token string) string {
	payload := jwtPayload(token)
	if payload == nil {
		return ""
	}
	lookup := func(values map[string]any) string {
		for _, key := range []string{"id", "userId", "userID", "user_id", "uid", "sub"} {
			if value, ok := values[key]; ok {
				if result := traeTokenString(value); result != "" {
					return result
				}
			}
		}
		return ""
	}
	if result := lookup(payload); result != "" {
		return result
	}
	if nested, ok := payload["data"].(map[string]any); ok {
		return lookup(nested)
	}
	return ""
}

func jwtExpiry(token string) time.Time {
	payload := jwtPayload(token)
	if payload == nil {
		return time.Time{}
	}
	return parseTraeTime(payload["exp"])
}

func traeCNExchangeHost(normalizedHost string) string {
	if strings.EqualFold(strings.TrimRight(strings.TrimSpace(normalizedHost), "/"), TraeCNDefaultHost) {
		return TraeCNAuthHost
	}
	return normalizedHost
}

// ExchangeTraeCNRefreshToken exchanges one RT for an AT. The optional Resin
// account ID keeps the token exchange on the same sticky Resin lease as the
// following inference request. The function remains Store-independent for
// admin probes and isolated callers.
func ExchangeTraeCNRefreshToken(ctx context.Context, refreshToken, host, proxyURL string, resinAccountID ...string) (TraeCNToken, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return TraeCNToken{}, fmt.Errorf("refresh_token is required")
	}
	normalizedHost, err := NormalizeTraeCNHost(host)
	if err != nil {
		return TraeCNToken{}, err
	}
	payload := map[string]string{
		"ClientID":     TraeCNOAuthClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	body, _ := json.Marshal(payload)
	exchangeHost := traeCNExchangeHost(normalizedHost)
	targetURL := exchangeHost + TraeCNExchangePath
	accountID := ""
	if len(resinAccountID) > 0 {
		accountID = strings.TrimSpace(resinAccountID[0])
	}
	targetURL, viaResin := ResinRequestURL(ctx, targetURL, accountID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(string(body)))
	if err != nil {
		return TraeCNToken{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if viaResin {
		req.Header.Set("X-Resin-Account", accountID)
	}
	client := &http.Client{Timeout: TraeCNExchangeTimeout}
	if viaResin {
		client = NewResinHTTPClient(TraeCNExchangeTimeout)
	}
	// Resin is the complete egress route, not an HTTP CONNECT proxy. Do not
	// wrap the Resin request in the account/global proxy selected for fallback.
	if proxyURL = strings.TrimSpace(proxyURL); !viaResin && proxyURL != "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if proxyErr := ConfigureTransportProxy(transport, proxyURL, nil); proxyErr != nil {
			return TraeCNToken{}, fmt.Errorf("invalid proxy URL: %w", proxyErr)
		}
		client.Transport = transport
	}
	resp, err := client.Do(req)
	if err != nil {
		return TraeCNToken{}, fmt.Errorf("ExchangeToken request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return TraeCNToken{}, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(raw))
		if len(message) > 512 {
			message = message[:512]
		}
		return TraeCNToken{}, fmt.Errorf("ExchangeToken failed: HTTP %d %s", resp.StatusCode, message)
	}
	return normalizeTraeTokenResponse(raw)
}

type traeCNDeviceProfile struct {
	MachineID      string
	DeviceID       string
	DeviceBrand    string
	DeviceCPU      string
	DeviceType     string
	OSVersion      string
	IDEVersion     string
	IDEVersionCode string
}

var (
	traeCNDeviceProfileOnce sync.Once
	traeCNDiscoveredProfile traeCNDeviceProfile
)

func firstTraeCNEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func traeCNCommand(path string, args ...string) string {
	if path == "" {
		return ""
	}
	raw, err := exec.Command(path, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func traeCNStorageCandidates() []string {
	paths := make([]string, 0, 6)
	if dataDir := firstTraeCNEnv("TRAECN_DATA_DIR", "TRAE_DATA_DIR"); dataDir != "" {
		paths = append(paths, filepath.Join(dataDir, "User", "globalStorage", "storage.json"))
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		for _, name := range []string{"Trae CN", "Trae-CN"} {
			paths = append(paths, filepath.Join(home, "Library", "Application Support", name, "User", "globalStorage", "storage.json"))
		}
	case "windows":
		root := strings.TrimSpace(os.Getenv("APPDATA"))
		if root == "" {
			root = filepath.Join(home, "AppData", "Roaming")
		}
		for _, name := range []string{"Trae CN", "Trae-CN"} {
			paths = append(paths, filepath.Join(root, name, "User", "globalStorage", "storage.json"))
		}
	default:
		root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
		if root == "" {
			root = filepath.Join(home, ".config")
		}
		for _, name := range []string{"Trae CN", "Trae-CN"} {
			paths = append(paths, filepath.Join(root, name, "User", "globalStorage", "storage.json"))
		}
	}
	return paths
}

func discoverTraeCNMachineID() string {
	for _, path := range traeCNStorageCandidates() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var storage map[string]any
		if json.Unmarshal(raw, &storage) != nil {
			continue
		}
		if machineID := traeTokenString(storage["telemetry.machineId"]); machineID != "" {
			return machineID
		}
	}
	return ""
}

func traeCNAppCandidates() []string {
	if appPath := firstTraeCNEnv("TRAECN_APP_PATH", "TRAE_APP_PATH"); appPath != "" {
		return []string{appPath}
	}
	if runtime.GOOS != "darwin" {
		return nil
	}
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join("/Applications", "Trae CN.app"),
		filepath.Join("/Applications", "Trae-CN.app"),
		filepath.Join(home, "Applications", "Trae CN.app"),
		filepath.Join(home, "Applications", "Trae-CN.app"),
	}
}

func discoverTraeCNIDE() (version, versionCode string) {
	for _, appPath := range traeCNAppCandidates() {
		if version == "" && runtime.GOOS == "darwin" {
			plist := filepath.Join(appPath, "Contents", "Info.plist")
			if _, err := os.Stat(plist); err == nil {
				version = traeCNCommand("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", plist)
			}
		}
		for _, relative := range []string{
			filepath.Join("Contents", "Resources", "app", "extensions", "ai-completion", "package.json"),
			filepath.Join("Contents", "Resources", "app", "product.json"),
		} {
			raw, err := os.ReadFile(filepath.Join(appPath, relative))
			if err != nil {
				continue
			}
			var metadata map[string]any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if decoder.Decode(&metadata) != nil {
				continue
			}
			versionCode = traeTokenString(metadata["versionCode"])
			if versionCode == "" {
				versionCode = traeTokenString(metadata["traeVersionCode"])
			}
			if versionCode != "" {
				break
			}
		}
		if version != "" || versionCode != "" {
			return version, versionCode
		}
	}
	return "", ""
}

func discoverTraeCNDeviceProfile() traeCNDeviceProfile {
	profile := traeCNDeviceProfile{MachineID: discoverTraeCNMachineID()}
	profile.IDEVersion, profile.IDEVersionCode = discoverTraeCNIDE()
	switch runtime.GOOS {
	case "darwin":
		profile.DeviceBrand = traeCNCommand("/usr/sbin/sysctl", "-n", "hw.model")
		if profile.DeviceBrand == "" {
			profile.DeviceBrand = "Mac"
		}
		profile.DeviceCPU = "Apple"
		profile.DeviceType = "mac"
		if version := traeCNCommand("/usr/bin/sw_vers", "-productVersion"); version != "" {
			profile.OSVersion = "macOS " + version
		} else {
			profile.OSVersion = "macOS"
		}
	case "windows":
		profile.DeviceBrand = "Windows PC"
		profile.DeviceCPU = runtime.GOARCH
		profile.DeviceType = "windows"
		profile.OSVersion = "Windows"
	default:
		profile.DeviceBrand = "Linux PC"
		profile.DeviceCPU = runtime.GOARCH
		profile.DeviceType = "linux"
		profile.OSVersion = "Linux"
	}
	return profile
}

// traeCNDeviceIDForMachineID mirrors the desktop client's 32-bit JavaScript
// string hash. Trae expects x-device-id and x-machine-id to be distinct: the
// former is this numeric derivative, while the latter is the telemetry ID.
// traeCNPreferredIDECode 选择要上报的客户端发布码：优先用本机 app 里发现的更新
// 值，否则回落到内置的真实发布码。YYYYMMDD 是定长数字，字典序即时间序。
func traeCNPreferredIDECode(discovered string) string {
	discovered = strings.TrimSpace(discovered)
	if discovered > strings.TrimSpace(TraeCNDefaultIDECode) {
		return discovered
	}
	return TraeCNDefaultIDECode
}

func traeCNDeviceIDForMachineID(machineID string) string {
	var hash int32
	for _, char := range machineID {
		hash = hash*31 + int32(char)
	}
	abs := int64(hash)
	if abs < 0 {
		abs = -abs
	}
	return fmt.Sprintf("%019d", abs)
}

// traeCNBaseDeviceProfile 是主机层面的画像：品牌/型号/CPU/系统/IDE 版本。这些字段
// 不唯一标识一台设备，多账号共用没有风控问题；唯一标识设备的 machine_id / device_id
// 由账号各自绑定（见 traecn_device.go），不从这里来。
func traeCNBaseDeviceProfile() traeCNDeviceProfile {
	traeCNDeviceProfileOnce.Do(func() {
		traeCNDiscoveredProfile = discoverTraeCNDeviceProfile()
	})
	profile := traeCNDiscoveredProfile
	// 主机探测到的 machine_id/device_id 是"运行网关这台机器"的标识，绝不能当成账号
	// 设备码：一个网关跑多个账号时会变成同机批量登录。只保留它自己派生出的 IDE 版本
	// 等信息，设备码由调用方绑定。
	profile.MachineID = ""
	profile.DeviceID = ""
	return profile
}

// traeCNApplyDeviceIdentity 把一份设备码写进画像（DeviceID 缺失时按 machine_id 派生）。
func traeCNApplyDeviceIdentity(profile traeCNDeviceProfile, identity TraeCNDeviceIdentity) traeCNDeviceProfile {
	if machineID := strings.TrimSpace(identity.MachineID); machineID != "" {
		profile.MachineID = machineID
	}
	if deviceID := strings.TrimSpace(identity.DeviceID); deviceID != "" {
		profile.DeviceID = deviceID
	}
	if profile.DeviceID == "" && profile.MachineID != "" {
		profile.DeviceID = traeCNDeviceIDForMachineID(profile.MachineID)
	}
	return profile
}

// traeCNDeviceProfileForAccount 组装某账号实际使用的设备画像：主机画像 + 账号绑定的
// 设备码（未绑定时按账号稳定身份确定性派生）+ 环境变量覆盖。
func traeCNDeviceProfileForAccount(account *Account, stableSeed string) traeCNDeviceProfile {
	if account == nil {
		return traeCNDeviceProfileForSeed(stableSeed)
	}
	identity, _ := account.traeCNBoundDeviceIdentity(stableSeed)
	profile := traeCNApplyDeviceIdentity(traeCNBaseDeviceProfile(), identity)
	return traeCNFinalizeDeviceProfile(profile)
}

func traeCNDeviceProfileForSeed(seed string) traeCNDeviceProfile {
	profile := traeCNApplyDeviceIdentity(traeCNBaseDeviceProfile(), DeriveTraeCNDeviceIdentity(seed))
	return traeCNFinalizeDeviceProfile(profile)
}

// traeCNFinalizeDeviceProfile 应用 TRAECN_*/TRAE_* 环境变量覆盖并补齐缺省字段。
func traeCNFinalizeDeviceProfile(profile traeCNDeviceProfile) traeCNDeviceProfile {
	if value := firstTraeCNEnv("TRAECN_MACHINE_ID", "TRAE_MACHINE_ID"); value != "" {
		profile.MachineID = value
	}
	if value := firstTraeCNEnv("TRAECN_DEVICE_ID", "TRAE_DEVICE_ID"); value != "" {
		profile.DeviceID = value
	}
	if value := firstTraeCNEnv("TRAECN_DEVICE_MODEL", "TRAECN_DEVICE_BRAND", "TRAE_DEVICE_MODEL", "TRAE_DEVICE_BRAND"); value != "" {
		profile.DeviceBrand = value
	}
	if value := firstTraeCNEnv("TRAECN_CPU", "TRAE_CPU"); value != "" {
		profile.DeviceCPU = value
	}
	if value := firstTraeCNEnv("TRAECN_OS_NAME", "TRAE_OS_NAME"); value != "" {
		profile.DeviceType = value
	}
	if value := firstTraeCNEnv("TRAECN_OS_VERSION", "TRAE_OS_VERSION"); value != "" {
		profile.OSVersion = value
	}
	if value := firstTraeCNEnv("TRAECN_IDE_VERSION", "TRAE_IDE_VERSION"); value != "" {
		profile.IDEVersion = value
	}
	explicitIDECode := false
	if value := firstTraeCNEnv("TRAECN_IDE_VERSION_CODE", "TRAE_IDE_VERSION_CODE"); value != "" {
		profile.IDEVersionCode = value
		explicitIDECode = true
	}
	if profile.DeviceBrand == "" {
		profile.DeviceBrand = "Mac"
	}
	if profile.DeviceCPU == "" {
		profile.DeviceCPU = "Apple"
	}
	if profile.DeviceType == "" {
		profile.DeviceType = "mac"
	}
	if profile.OSVersion == "" {
		profile.OSVersion = "macOS"
	}
	if profile.IDEVersion == "" {
		profile.IDEVersion = TraeCNDefaultIDE
	}
	if profile.IDEVersionCode == "" {
		profile.IDEVersionCode = TraeCNDefaultIDECode
	}
	// 未被显式覆盖时对齐到真实的客户端发布码（客户端升级后由常量或 env 跟进）。
	if !explicitIDECode {
		profile.IDEVersionCode = traeCNPreferredIDECode(profile.IDEVersionCode)
	}
	return profile
}

// TraeCNRequestHeaders builds the provider-specific headers for one request.
// It prefers the local Trae desktop telemetry profile (or TRAECN_*/TRAE_* env
// overrides) because synthetic and internally inconsistent IDE/device values
// are surfaced by Trae as SSE code 4011. If no desktop profile is available,
// a stable per-credential fallback keeps retries on one coherent fingerprint.
func TraeCNRequestHeaders(account *Account, accessToken, requestID string) http.Header {
	if requestID == "" {
		requestID = uuid.NewString()
	}
	seed, userID := traeCNHeaderIdentity(account, requestID)
	profile := traeCNDeviceProfileForAccount(account, seed)
	traceID := strings.ReplaceAll(uuid.NewString(), "-", "")
	appID := firstTraeCNEnv("TRAECN_APP_ID", "TRAE_APP_ID")
	if appID == "" {
		appID = TraeCNAppID
	}
	userAgent := firstTraeCNEnv("TRAECN_USER_AGENT", "TRAE_USER_AGENT")
	if userAgent == "" {
		userAgent = TraeCNDefaultUserAgent
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("Authorization", "Cloud-IDE-JWT "+strings.TrimSpace(accessToken))
	headers.Set("X-Cloudide-Token", strings.TrimSpace(accessToken))
	headers.Set("x-app-id", appID)
	headers.Set("x-app-version", "default")
	headers.Set("x-ide-version-code", profile.IDEVersionCode)
	headers.Set("x-app-version-code", profile.IDEVersionCode)
	headers.Set("x-custom-trace-id", traceID)
	headers.Set("x-device-brand", profile.DeviceBrand)
	headers.Set("x-device-cpu", profile.DeviceCPU)
	headers.Set("x-device-id", profile.DeviceID)
	headers.Set("x-machine-id", profile.MachineID)
	headers.Set("x-os-version", profile.OSVersion)
	headers.Set("x-device-type", profile.DeviceType)
	headers.Set("x-ide-version", profile.IDEVersion)
	headers.Set("x-ide-version-type", "stable")
	headers.Set("request-traffic-type", "prod")
	headers.Set("User-Agent", userAgent)
	headers.Set("x-uid", userID)
	headers.Set("X-Request-ID", requestID)
	headers.Set("X-Trae-Request-ID", requestID)
	return headers
}

// traeCNHeaderIdentity 派生每个账号稳定的设备种子与 user id。账号无 DB 身份时
// 用 RT 摘要兜底，绝不把凭据本身放进请求头。
func traeCNHeaderIdentity(account *Account, requestID string) (seed, userID string) {
	seed = requestID
	if account == nil {
		return seed, ""
	}
	account.mu.RLock()
	userID = strings.TrimSpace(account.TraeCNUserID)
	stableIdentity := strings.TrimSpace(account.CredentialFamilyID)
	if stableIdentity == "" {
		stableIdentity = strings.TrimSpace(account.AccountID)
	}
	if stableIdentity == "" && account.DBID > 0 {
		stableIdentity = strconv.FormatInt(account.DBID, 10)
	}
	refreshToken := strings.TrimSpace(account.RefreshToken)
	account.mu.RUnlock()
	if stableIdentity != "" {
		return "traecn:" + stableIdentity, userID
	}
	if stable := TraeCNStableDeviceSeed("", "", account.DBID, refreshToken); stable != "" {
		return stable, userID
	}
	return seed, userID
}

// TraeCNStableDeviceSeed 是"账号稳定身份"的唯一定义：credential family -> 账号
// 业务 ID -> DBID -> RT 摘要。出站请求指纹、未绑定账号的设备码、账号列表展示
// 都必须用它，换算法就等于给账号换设备。
func TraeCNStableDeviceSeed(familyID, accountID string, dbID int64, refreshToken string) string {
	if value := strings.TrimSpace(familyID); value != "" {
		return "traecn:" + value
	}
	if value := strings.TrimSpace(accountID); value != "" {
		return "traecn:" + value
	}
	if dbID > 0 {
		return "traecn:" + strconv.FormatInt(dbID, 10)
	}
	if value := strings.TrimSpace(refreshToken); value != "" {
		digest := sha256.Sum256([]byte(value))
		return hex.EncodeToString(digest[:])
	}
	return ""
}

// refreshTraeCNAccount exchanges the account RT under a per-account mutex and
// persists the rotated credential atomically. This mirrors the existing OAuth
// refresh lifecycle while keeping Trae's non-OpenAI token endpoint isolated.
func (s *Store) refreshTraeCNAccount(ctx context.Context, account *Account, forceRefresh bool) error {
	return s.refreshTraeCNAccountWithProxy(ctx, account, forceRefresh, nil)
}

// reloadTraeCNCredentialsAfterFamilyLease refreshes the in-memory credential
// projection from the authoritative row after a cross-instance family lease is
// acquired.  A different process may have consumed the previous rotating RT
// while this process was waiting; reusing its committed AT is both safer and
// cheaper than consuming the newly-issued RT a second time.
func (s *Store) reloadTraeCNCredentialsAfterFamilyLease(ctx context.Context, account *Account, lockedAccessToken, lockedRefreshToken string) (changed bool, usable bool, err error) {
	if s == nil || s.db == nil || account == nil || account.DBID <= 0 {
		return false, false, nil
	}
	row, err := s.db.GetAccountByID(ctx, account.DBID)
	if err != nil {
		return false, false, err
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), UpstreamTraeCN) {
		return false, false, fmt.Errorf("账号 %d 不是 Trae CN 账号", account.DBID)
	}
	refreshToken := strings.TrimSpace(row.GetCredential("refresh_token"))
	accessToken := strings.TrimSpace(row.GetCredential("access_token"))
	rowHost := strings.TrimSpace(row.GetCredential("traecn_host"))
	rowUserID := strings.TrimSpace(row.GetCredential("traecn_user_id"))
	rowAccountID := strings.TrimSpace(row.GetCredential("account_id"))
	if rowAccountID == "" {
		rowAccountID = rowUserID
	}
	rowEmail := strings.TrimSpace(row.GetCredential("email"))
	rowPlanType := strings.TrimSpace(row.GetCredential("plan_type"))
	if rowPlanType == "" {
		rowPlanType = "traecn"
	}
	rowModels := normalizeModelList(row.GetCredentialStringSlice("models"))
	rowUpstreamModels := normalizeModelList(row.GetCredentialStringSlice(TraeCNUpstreamModelsCredentialKey))
	rowAllowlist := normalizeModelList(row.GetCredentialStringSlice(TraeCNModelAllowlistCredentialKey))
	rowAllowlistSet := row.GetCredentialBool(TraeCNModelAllowlistSetCredentialKey) || len(rowAllowlist) > 0
	rowSyncedAt := time.Time{}
	if raw := strings.TrimSpace(row.GetCredential(TraeCNModelsSyncedAtCredentialKey)); raw != "" {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, raw); parseErr == nil {
			rowSyncedAt = parsed.UTC()
		}
	}
	// Rows written before the dedicated Trae catalog fields existed used the
	// generic models field as an optional allowlist. Keep that migration rule in
	// the cross-instance reload path too; otherwise a lease hand-off could
	// silently widen or empty the account's effective model catalog.
	if !rowAllowlistSet && len(rowUpstreamModels) == 0 && len(rowAllowlist) == 0 {
		rowAllowlist = traeCNLegacyModelAllowlist(rowModels)
	}
	rowEffectiveModels := rowUpstreamModels
	if len(rowEffectiveModels) == 0 {
		rowEffectiveModels = TraeCNDefaultModelIDs()
	}
	rowEffectiveModels = traeCNModelIntersection(rowEffectiveModels, rowAllowlist)
	account.mu.RLock()
	currentGeneration := account.CredentialGeneration
	currentFamilyID := strings.TrimSpace(account.CredentialFamilyID)
	currentHost := strings.TrimSpace(account.TraeCNHost)
	currentUserID := strings.TrimSpace(account.TraeCNUserID)
	currentAccountID := strings.TrimSpace(account.AccountID)
	currentEmail := strings.TrimSpace(account.Email)
	currentPlanType := strings.TrimSpace(account.PlanType)
	currentModels := cloneStringSlice(account.Models)
	currentUpstreamModels := cloneStringSlice(account.TraeCNUpstreamModelCatalog)
	currentAllowlist := cloneStringSlice(account.TraeCNModelAllowlist)
	currentAllowlistSet := account.TraeCNModelAllowlistSet
	currentSyncedAt := account.TraeCNModelCatalogSyncedAtValue
	currentProxyURL := strings.TrimSpace(account.ProxyURL)
	account.mu.RUnlock()
	rowGeneration := row.CredentialGeneration
	if rowGeneration <= 0 {
		rowGeneration = 1
	}
	rowFamilyID := strings.TrimSpace(row.CredentialFamilyID)
	changed = refreshToken != strings.TrimSpace(lockedRefreshToken) ||
		accessToken != strings.TrimSpace(lockedAccessToken) ||
		rowGeneration != currentGeneration ||
		(rowFamilyID != "" && rowFamilyID != currentFamilyID)
	projectionChanged := rowHost != currentHost ||
		rowUserID != currentUserID ||
		rowAccountID != currentAccountID ||
		rowEmail != currentEmail ||
		rowPlanType != currentPlanType ||
		!traeCNModelListsEqual(rowEffectiveModels, currentModels) ||
		!traeCNModelListsEqual(rowUpstreamModels, currentUpstreamModels) ||
		!traeCNModelListsEqual(rowAllowlist, currentAllowlist) ||
		rowAllowlistSet != currentAllowlistSet ||
		!rowSyncedAt.Equal(currentSyncedAt) ||
		strings.TrimSpace(row.ProxyURL) != currentProxyURL
	if !changed && !projectionChanged {
		return false, false, nil
	}
	expiresAt := parseOAuthCredentialExpiry(row.GetCredential("expires_at"))
	account.mu.Lock()
	// Reload the complete Trae identity projection before deciding whether a
	// waiting caller may reuse the result. This also clears fields when an
	// administrator replaced a credential with an empty value.
	account.UpstreamType = strings.TrimSpace(row.GetCredential("upstream_type"))
	account.RefreshToken = refreshToken
	account.AccessToken = accessToken
	account.ExpiresAt = expiresAt
	account.TraeCNHost = rowHost
	account.TraeCNUserID = rowUserID
	account.AccountID = rowAccountID
	if account.AccountID == "" {
		account.AccountID = account.TraeCNUserID
	}
	account.Email = rowEmail
	account.PlanType = rowPlanType
	account.TraeCNUpstreamModelCatalog = rowUpstreamModels
	account.TraeCNModelAllowlist = rowAllowlist
	account.TraeCNModelAllowlistSet = rowAllowlistSet
	account.TraeCNModelCatalogSyncedAtValue = rowSyncedAt
	account.Models = rowModels
	// Recompute the legacy projection from the same catalog/allowlist pair used
	// by normal account construction. This keeps routing and admin output in
	// sync after a lease owner updates the catalog in another process.
	account.Models = traeCNEffectiveModelsLocked(account)
	account.ProxyURL = strings.TrimSpace(row.ProxyURL)
	account.CredentialGeneration = rowGeneration
	if rowFamilyID != "" {
		account.CredentialFamilyID = rowFamilyID
	}
	account.mu.Unlock()
	usable = accessToken != "" && (expiresAt.IsZero() || time.Until(expiresAt) > TraeCNAccessTokenGrace)
	return changed, usable, nil
}

// ensureTraeCNFamilyLease obtains the stable credential-family lease used to
// serialize rotating Trae refresh tokens across processes.  New imports already
// receive a family id; legacy rows are initialized once and never reassigned.
func (s *Store) ensureTraeCNFamilyLease(ctx context.Context, account *Account) (*oauthRefreshLease, error) {
	if s == nil || s.db == nil || account == nil || account.DBID <= 0 {
		return nil, nil
	}
	account.mu.RLock()
	familyID := strings.TrimSpace(account.CredentialFamilyID)
	account.mu.RUnlock()
	if familyID == "" {
		var err error
		familyID, err = s.db.EnsureAccountCredentialFamilyID(ctx, account.DBID, "")
		if err != nil {
			return nil, fmt.Errorf("初始化 Trae CN credential family 失败: %w", err)
		}
		familyID = strings.TrimSpace(familyID)
		if familyID == "" {
			return nil, fmt.Errorf("初始化 Trae CN credential family 失败: family id 为空")
		}
		account.mu.Lock()
		account.CredentialFamilyID = familyID
		account.mu.Unlock()
	}
	// Keep the namespace distinct from Grok even though both use the generic
	// OAuth lease implementation; the same family id must never collide across
	// providers or accidentally serialize unrelated credentials.
	lease, err := s.acquireOAuthRefreshLease(ctx, "traecn-family:"+familyID)
	if err != nil {
		return nil, fmt.Errorf("获取 Trae CN OAuth 刷新 lease 失败: %w", err)
	}
	return lease, nil
}

// refreshTraeCNAccountWithProxy serializes exchange, durable publication and
// in-memory publication as one account operation. Resin, when configured,
// owns the egress and uses the account DBID as its lease key. Otherwise,
// proxyOverride keeps a 401 retry on the exact forward proxy selected for that
// request; a nil override resolves the current sticky account proxy policy.
func (s *Store) refreshTraeCNAccountWithProxy(ctx context.Context, account *Account, forceRefresh bool, proxyOverride *string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || account == nil {
		return fmt.Errorf("traecn account is unavailable")
	}
	account.mu.RLock()
	observedAccessToken := strings.TrimSpace(account.AccessToken)
	observedRefreshToken := strings.TrimSpace(account.RefreshToken)
	account.mu.RUnlock()

	account.traeRefreshMu.Lock()
	defer account.traeRefreshMu.Unlock()

	account.mu.RLock()
	accessToken := strings.TrimSpace(account.AccessToken)
	expiresAt := account.ExpiresAt
	refreshToken := strings.TrimSpace(account.RefreshToken)
	host := strings.TrimSpace(account.TraeCNHost)
	if host == "" {
		host = strings.TrimSpace(account.BaseURL)
	}
	dbID := account.DBID
	expectedGeneration := account.CredentialGeneration
	account.mu.RUnlock()
	if expectedGeneration <= 0 {
		expectedGeneration = 1
	}

	// A stable family lease closes the multi-instance rotation window. The
	// per-account mutex above remains useful for same-process callers and keeps
	// the common no-database/transient path lightweight.
	var familyLease *oauthRefreshLease
	if refreshToken != "" && dbID > 0 && s.db != nil {
		var leaseErr error
		familyLease, leaseErr = s.ensureTraeCNFamilyLease(ctx, account)
		if leaseErr != nil {
			// If the database has already been closed, the durable publication
			// below is still the authoritative failure point. Continue through
			// the exchange so the caller gets the persistence error and, more
			// importantly, never publish the rotated credential to memory. Other
			// family/lease errors remain fail-closed because silently falling back
			// could allow two instances to consume the same rotating RT.
			if !strings.Contains(strings.ToLower(leaseErr.Error()), "database is closed") &&
				!strings.Contains(strings.ToLower(leaseErr.Error()), "closed database") {
				return leaseErr
			}
			log.Printf("Trae CN credential family lease skipped because database is closed: %v", leaseErr)
		}
		if familyLease != nil {
			defer familyLease.Release()
			changed, usable, reloadErr := s.reloadTraeCNCredentialsAfterFamilyLease(familyLease.Context(), account, observedAccessToken, observedRefreshToken)
			if reloadErr != nil {
				// A closed database cannot participate in the advisory reload, but
				// the exchange below will still surface the required durable-write
				// error. Keep the lease fallback limited to this terminal database
				// state; all other reload failures must fail closed.
				if !strings.Contains(strings.ToLower(reloadErr.Error()), "database is closed") &&
					!strings.Contains(strings.ToLower(reloadErr.Error()), "closed database") {
					return fmt.Errorf("读取 Trae CN 刷新后的凭据失败: %w", reloadErr)
				}
				log.Printf("Trae CN credential reload skipped because database is closed: %v", reloadErr)
				familyLease = nil
			} else if changed && usable {
				// Both proactive and forced refresh callers can reuse the fresh
				// credential committed by the lease owner. This is especially
				// important after a 401, where rotating again would invalidate it.
				s.finishReloadedOAuthRefresh(familyLease.CriticalContext(), account)
				return nil
			}
			if familyLease != nil {
				account.mu.RLock()
				accessToken = strings.TrimSpace(account.AccessToken)
				expiresAt = account.ExpiresAt
				refreshToken = strings.TrimSpace(account.RefreshToken)
				host = strings.TrimSpace(account.TraeCNHost)
				if host == "" {
					host = strings.TrimSpace(account.BaseURL)
				}
				dbID = account.DBID
				expectedGeneration = account.CredentialGeneration
				account.mu.RUnlock()
				if expectedGeneration <= 0 {
					expectedGeneration = 1
				}
			}
		}
	}

	// Concurrent 401s may all ask for a forced refresh after observing the same
	// rejected token. Once the first caller publishes a newer credential, later
	// callers reuse it instead of rotating the RT repeatedly.
	credentialsChangedWhileWaiting := accessToken != observedAccessToken || refreshToken != observedRefreshToken
	if forceRefresh && credentialsChangedWhileWaiting && accessToken != "" && (expiresAt.IsZero() || time.Now().Before(expiresAt)) {
		return nil
	}
	if !forceRefresh && accessToken != "" {
		if expiresAt.IsZero() || time.Until(expiresAt) > TraeCNAccessTokenGrace {
			return nil
		}
		// An AT-only import cannot refresh. Keep using a token that is still
		// valid even when it has entered the proactive refresh grace window.
		if refreshToken == "" && time.Now().Before(expiresAt) {
			return nil
		}
	}
	if refreshToken == "" {
		if accessToken != "" && !expiresAt.IsZero() && !time.Now().Before(expiresAt) {
			return fmt.Errorf("traecn access_token is expired and refresh_token is empty")
		}
		return fmt.Errorf("traecn refresh_token is empty")
	}

	proxyURL := ""
	if proxyOverride != nil {
		proxyURL = strings.TrimSpace(*proxyOverride)
		if proxyURL == "" && s.GetProxyPoolEnabled() {
			return fmt.Errorf("账号 %d 代理池已启用但当前请求没有可用代理，已拒绝直连刷新", dbID)
		}
	} else {
		var usable bool
		proxyURL, usable = s.ResolveUsableProxyForAccount(account)
		if !usable {
			return fmt.Errorf("账号 %d 代理池已启用但无可用代理，已拒绝直连刷新", dbID)
		}
	}
	// ExchangeToken and the following durable write form one RT-consumption
	// critical section. Do not let a disconnected browser/request cancel it.
	criticalCtx := context.WithoutCancel(ctx)
	var criticalCancel context.CancelFunc
	if familyLease != nil {
		criticalCtx = familyLease.CriticalContext()
	} else {
		criticalCtx, criticalCancel = context.WithTimeout(criticalCtx, TraeCNRefreshCriticalTimeout)
		defer criticalCancel()
	}
	resinAccountID := ""
	if dbID > 0 {
		resinAccountID = strconv.FormatInt(dbID, 10)
	}
	token, err := ExchangeTraeCNRefreshToken(criticalCtx, refreshToken, host, proxyURL, resinAccountID)
	if err != nil {
		return err
	}
	if token.AccessToken == "" {
		return fmt.Errorf("traecn ExchangeToken returned an empty access token")
	}
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}

	// Publish rotated credentials to durable storage before making them visible
	// to request dispatch. Returning the write error prevents a successful HTTP
	// exchange from being reported as durable when the database still has the
	// rejected/expired token.
	if s.db != nil && dbID > 0 {
		credentials := map[string]interface{}{
			"upstream_type": UpstreamTraeCN,
			"access_token":  token.AccessToken,
			"refresh_token": token.RefreshToken,
			"expires_at":    token.ExpiresAt.UTC().Format(time.RFC3339),
		}
		if token.UserID != "" {
			credentials["traecn_user_id"] = token.UserID
			credentials["account_id"] = token.UserID
			credentials["email"] = token.UserID
		}
		newGeneration, applied, casErr := s.db.UpdateAccountCredentialsCAS(criticalCtx, dbID, expectedGeneration, credentials)
		if casErr != nil {
			return fmt.Errorf("持久化 Trae CN credentials 失败: %w", casErr)
		}
		if !applied {
			// An administrator (or another instance) replaced the credential after
			// this refresh started. Never publish the provider result obtained from
			// the old RT. Reload the authoritative row and reuse it only when it is
			// already dispatchable; otherwise surface a retryable refresh failure.
			changed, usable, reloadErr := s.reloadTraeCNCredentialsAfterFamilyLease(criticalCtx, account, accessToken, refreshToken)
			if reloadErr != nil {
				s.RemoveAccount(dbID)
				return fmt.Errorf("读取 Trae CN 刷新后的凭据失败: %w", reloadErr)
			}
			if changed && usable {
				s.finishReloadedOAuthRefresh(criticalCtx, account)
				return nil
			}
			return fmt.Errorf("Trae CN credential generation changed during refresh; discarded stale provider result")
		}
		expectedGeneration = newGeneration
	}

	account.mu.Lock()
	account.AccessToken = token.AccessToken
	account.RefreshToken = token.RefreshToken
	account.ExpiresAt = token.ExpiresAt
	if expectedGeneration > 0 {
		account.CredentialGeneration = expectedGeneration
	}
	if token.UserID != "" {
		account.TraeCNUserID = token.UserID
		account.AccountID = token.UserID
		if strings.TrimSpace(account.Email) == "" {
			account.Email = token.UserID
		}
	}
	account.ErrorMsg = ""
	account.PermanentRefreshFailures = 0
	activeCooldown := account.Status == StatusCooldown && time.Now().Before(account.CooldownUtil)
	if !activeCooldown {
		account.Status = StatusReady
		account.CooldownUtil = time.Time{}
		account.CooldownReason = ""
		if account.HealthTier == HealthTierRisky || account.HealthTier == "" {
			account.HealthTier = HealthTierHealthy
		}
	}
	account.recomputeSchedulerLocked(int64(s.GetMaxConcurrency()))
	account.mu.Unlock()
	s.fastSchedulerUpdate(account)

	if s.db != nil && dbID > 0 {
		if err := s.db.ClearError(criticalCtx, dbID); err != nil {
			log.Printf("[账号 %d] 清理 Trae CN 错误状态失败: %v", dbID, err)
		}
	}
	return nil
}

// EnsureTraeCNAccount makes the account dispatch-ready and persists a rotated
// RT/AT pair when a lazy refresh was required. Request executors also perform a
// defensive in-memory check, but production handlers use this Store-owned path
// so a provider that rotates refresh tokens cannot leave only the old RT on
// disk.
func (s *Store) EnsureTraeCNAccount(ctx context.Context, account *Account) error {
	if s == nil || account == nil || !account.IsTraeCNAPI() {
		return fmt.Errorf("traecn account is unavailable")
	}
	return s.refreshTraeCNAccount(ctx, account, false)
}

// EnsureTraeCNAccountWithProxy is the request-path variant that pins lazy
// refresh to the exact egress selected for the subsequent upstream call. An
// empty proxy is meaningful: when the proxy pool is enabled it fails closed,
// otherwise it permits direct egress just like the normal resolver.
func (s *Store) EnsureTraeCNAccountWithProxy(ctx context.Context, account *Account, proxyURL string) error {
	if s == nil || account == nil || !account.IsTraeCNAPI() {
		return fmt.Errorf("traecn account is unavailable")
	}
	proxyURL = strings.TrimSpace(proxyURL)
	return s.refreshTraeCNAccountWithProxy(ctx, account, false, &proxyURL)
}

// RefreshTraeCNAccountByID refreshes an explicitly addressed Trae CN account.
func (s *Store) RefreshTraeCNAccountByID(ctx context.Context, dbID int64) error {
	return s.refreshTraeCNAccountByID(ctx, dbID, nil)
}

// RefreshTraeCNAccountByIDWithProxy forces refresh over the already-selected
// request proxy. It is used only for an upstream 401 retry and avoids changing
// Trae identity egress between the failed request and its token exchange.
func (s *Store) RefreshTraeCNAccountByIDWithProxy(ctx context.Context, dbID int64, proxyURL string) error {
	proxyURL = strings.TrimSpace(proxyURL)
	return s.refreshTraeCNAccountByID(ctx, dbID, &proxyURL)
}

func (s *Store) refreshTraeCNAccountByID(ctx context.Context, dbID int64, proxyOverride *string) error {
	if s == nil || dbID <= 0 {
		return fmt.Errorf("账号 %d 不存在", dbID)
	}
	account := s.FindByID(dbID)
	if account == nil {
		var err error
		account, err = s.BuildTransientAccountByID(ctx, dbID)
		if err != nil {
			return err
		}
	}
	if !account.IsTraeCNAPI() {
		return fmt.Errorf("账号 %d 不是 Trae CN 账号", dbID)
	}
	return s.refreshTraeCNAccountWithProxy(ctx, account, true, proxyOverride)
}
