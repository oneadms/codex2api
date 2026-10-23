package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// CodexGatewayWSError is terminal for a fixed session. In particular it must
// never be handled as an ordinary continuation loss that permits replay.
type CodexGatewayWSError struct {
	Reason string
	Cause  error
}

func (e *CodexGatewayWSError) Unwrap() error { return e.Cause }
func (e *CodexGatewayWSError) Error() string { return "固定 WebSocket 会话失败: " + e.Reason }

type codexGatewayWSClientScopeKey struct{}

// codexSyntheticProbeKey marks request contexts produced by internal synthetic
// probes (admin connection tests, batch tests, model probes). Those probes
// have no downstream API key and no explicit client session/thread identity,
// so they can never own a fixed gateway WS anchor. They must fall through to
// the ordinary transport instead of failing on the anchor admission check.
type codexSyntheticProbeKey struct{}

// WithCodexSyntheticProbe declares that this request is an internal synthetic
// probe rather than authenticated business traffic.
func WithCodexSyntheticProbe(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexSyntheticProbeKey{}, true)
}

// codexSyntheticProbeFromContext reports whether the request context was
// marked as an internal synthetic probe.
func codexSyntheticProbeFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	probe, _ := ctx.Value(codexSyntheticProbeKey{}).(bool)
	return probe
}

// Capture the client identity before fingerprint/payload/cache-key rewriting.
// An explicitly captured empty scope must stay empty throughout the attempt.
func withCodexGatewayWSClientScope(ctx context.Context, headers http.Header, body []byte) context.Context {
	if _, captured := ctx.Value(codexGatewayWSClientScopeKey{}).(string); captured {
		return ctx
	}
	return context.WithValue(ctx, codexGatewayWSClientScopeKey{}, codexGatewayWSClientScope(headers, body))
}

func codexGatewayWSClientScope(headers http.Header, body []byte) string {
	session, thread := extractClientCodexIdentity(headers)
	if thread == "" {
		thread = strings.TrimSpace(headers.Get("Thread_id"))
	}
	if thread == "" {
		thread = strings.TrimSpace(strings.SplitN(headers.Get("X-Codex-Window-Id"), ":", 2)[0])
	}
	if thread == "" {
		thread = strings.TrimSpace(gjson.GetBytes(body, "client_metadata.thread_id").String())
	}
	if thread == "" {
		embedded := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
		thread = strings.TrimSpace(gjson.Get(embedded, "thread_id").String())
	}
	kind, identity := "thread", thread
	if identity == "" {
		if session == "" {
			session = strings.TrimSpace(gjson.GetBytes(body, "client_metadata.session_id").String())
		}
		if session == "" {
			session = ResolveExplicitSessionID(headers, body)
		}
		kind, identity = "session", session
	}
	if identity == "" {
		return ""
	}
	// Guardian requests can carry a thread header but no thread in their turn
	// metadata; match the upstream execution-lane rule for those side requests.
	metadata := strings.TrimSpace(headers.Get(codexTurnMetadataHeader))
	if !gjson.Valid(metadata) {
		metadata = gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	}
	metadataThread := strings.TrimSpace(gjson.Get(metadata, "thread_id").String())
	// JSON encoding prevents ambiguous delimiter-bearing client identities.
	raw, _ := json.Marshal([]string{kind, identity, resolveCodexExecutionLane(headers, body, metadataThread)})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CodexGatewayWSRequired is also used by native ingress before its ordinary
// replay/fallback logic, not just by the low-level connection helper.
func CodexGatewayWSRequired(ctx context.Context, account *auth.Account, model string) (bool, error) {
	m := codexHarvester.Load()
	model = auth.NormalizeCodexTicketModel(model)
	if m == nil || account == nil || !account.SupportsCodexTickets() || (model != gatewayModel && !auth.CodexTicketModelGated(model)) {
		return false, nil
	}
	// 内部合成探针（测连/批量测试/模型探测）没有下游 API Key 和显式客户端
	// 会话标识，永远拿不到固定会话锚点。这里直接判 false，让它们回落普通
	// 转发路径，而不是在锚点准入处报“需要 API Key 和显式客户端会话标识”。
	if codexSyntheticProbeFromContext(ctx) {
		return false, nil
	}
	settings, err := m.GatewaySettings(ctx)
	if err != nil {
		return false, err
	}
	return settings.WSSession.Enabled && slices.Contains(settings.WSSession.AccountIDs, account.ID()), nil
}
