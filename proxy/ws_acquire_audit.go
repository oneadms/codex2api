package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// ws_acquire_ms（issue #413 跟进）：记录一次 attempt 内获取上游连接花费的
// 墙钟时间，挂在下游请求 ctx 上，由 wsrelay/TRAE CN 累加、logUsageForRequest
// 统一回填 usage_logs，使 first_token_ms 中「网关内取连排队」与「上游生成」
// 可分离归因。
//
// 两条路径的口径：
//   - Codex WS：busy 排队 + 探活 + 握手（含发送失败后的重建）。
//   - TRAE CN 等 HTTP 中转：请求转换、懒刷新令牌/代理租约、客户端池选择，
//     以及拿到可用连接的耗时（连接池命中约等于 0，冷连接包含拨号/TLS/代理
//     CONNECT；uTLS 传输由 utlsRoundTripper 单独记录）。
//
// 二者都不包含上游生成首内容的时间，因此 first_token_ms - ws_acquire_ms 是
// 「上游产出首个内容」的口径。其余未接入的 HTTP 路径仍为 0。与 userAgentAudit
// 同构：每个 attempt 由 executor 入口清零，最终落库的是成功（或写了日志行的）
// 那个 attempt 的取值。
type wsAcquireAuditContextKey struct{}

type wsAcquireAudit struct {
	mu    sync.Mutex
	total time.Duration
}

func withWsAcquireAudit(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if wsAcquireAuditFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, wsAcquireAuditContextKey{}, &wsAcquireAudit{})
}

func wsAcquireAuditFromContext(ctx context.Context) *wsAcquireAudit {
	if ctx == nil {
		return nil
	}
	audit, _ := ctx.Value(wsAcquireAuditContextKey{}).(*wsAcquireAudit)
	return audit
}

func resetWsAcquireAudit(ctx context.Context) {
	if audit := wsAcquireAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.total = 0
		audit.mu.Unlock()
	}
}

// AddWsAcquireDuration 累加一次 WS 连接获取耗时（wsrelay 调用；一个 attempt 内
// 发送失败重建连接会多次获取，累加取整体）。
func AddWsAcquireDuration(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	if audit := wsAcquireAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.total += d
		audit.mu.Unlock()
	}
}

func wsAcquireAuditMs(ctx context.Context) int {
	audit := wsAcquireAuditFromContext(ctx)
	if audit == nil {
		return 0
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	return int(audit.total.Milliseconds())
}

// wsAcquireAuditTotal 返回未取整的取连总耗时，供内部判断与测试使用。
func wsAcquireAuditTotal(ctx context.Context) time.Duration {
	audit := wsAcquireAuditFromContext(ctx)
	if audit == nil {
		return 0
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	return audit.total
}

func attachWsAcquireAudit(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(withWsAcquireAudit(c.Request.Context()))
}

func populateWsAcquireFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	input.WsAcquireMs = wsAcquireAuditMs(c.Request.Context())
	// 可选口径（默认关）：first_token_ms 扣除取连耗时，只保留"上游生成首内容"部分，
	// 与 HTTP 路径及取连恒为 0 的部署可比；原始值 = first_token_ms + ws_acquire_ms。
	// 已打点的行扣除后下限钳 1ms，避免归零后被前端当作"未记录"显示为 -。
	if CurrentRuntimeSettings().FirstTokenExcludesWsAcquire &&
		input.FirstTokenMs > 0 && input.WsAcquireMs > 0 {
		input.FirstTokenMs = max(input.FirstTokenMs-input.WsAcquireMs, 1)
	}
}
