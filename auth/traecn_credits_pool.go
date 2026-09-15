package auth

import (
	"strings"
	"time"
)

// Trae CN 的积分按“端点”分池：权益包里的 available_endpoint=0 是 IDE/Code 侧可用
// 额度，=1 是 Work 专属额度，两者分开消耗。推理请求用 body 里的 access_type 选择
// 端点（实测 access_type=1 扣 Work 池，缺省/0 扣 IDE 池），所以网关必须知道
// 每个账号当前该走哪个池。
const (
	// TraeCNCreditsPoolCredentialKey 是账号级积分池设置：auto（默认）/ code / work。
	TraeCNCreditsPoolCredentialKey = "traecn_credits_pool"

	TraeCNCreditsPoolAuto = "auto"
	TraeCNCreditsPoolCode = "code"
	TraeCNCreditsPoolWork = "work"

	// TraeCNWorkAccessType 是 llm_utils_chat 里选择 Work 端点的取值（实测 1、2 都扣
	// Work 池，1 是 Lite/Work 客户端的默认值）。
	TraeCNWorkAccessType = 1

	// traeCNCreditsBalanceTTL 是余额快照的有效期：过期就回到 IDE 池，避免一直用
	// 可能已经补满的旧结论。
	traeCNCreditsBalanceTTL = 15 * time.Minute
)

// NormalizeTraeCNCreditsPoolMode 把未知取值收敛到 auto。
func NormalizeTraeCNCreditsPoolMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case TraeCNCreditsPoolCode:
		return TraeCNCreditsPoolCode
	case TraeCNCreditsPoolWork:
		return TraeCNCreditsPoolWork
	default:
		return TraeCNCreditsPoolAuto
	}
}

// TraeCNCreditsPoolMode 返回账号的积分池设置（未知或未设置都是 auto）。
func (a *Account) TraeCNCreditsPoolMode() string {
	if a == nil {
		return TraeCNCreditsPoolAuto
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizeTraeCNCreditsPoolMode(a.TraeCNCreditsPool)
}

// TraeCNCreditsBalance 是最近一次观测到的两个端点余额。
type TraeCNCreditsBalance struct {
	CodeRemaining float64
	WorkRemaining float64
	ObservedAt    time.Time
}

// Known 报告这份余额是否还在有效期内。
func (b TraeCNCreditsBalance) Known(now time.Time) bool {
	return !b.ObservedAt.IsZero() && now.Sub(b.ObservedAt) < traeCNCreditsBalanceTTL
}

// TraeCNCreditsBalance 返回最近一次观测到的双端点余额。
func (a *Account) TraeCNCreditsBalance() TraeCNCreditsBalance {
	if a == nil {
		return TraeCNCreditsBalance{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.traeCNCreditsBalance
}

// SetTraeCNCreditsBalance 记录余额观测结果，供后续请求选择积分池。
func (a *Account) SetTraeCNCreditsBalance(balance TraeCNCreditsBalance) {
	if a == nil || balance.ObservedAt.IsZero() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.traeCNCreditsBalance = balance
}

// SetTraeCNCreditsBalanceFromPools 用两个池的剩余量更新余额快照。
func (a *Account) SetTraeCNCreditsBalanceFromPools(codeRemaining, workRemaining float64, now time.Time) {
	a.SetTraeCNCreditsBalance(TraeCNCreditsBalance{
		CodeRemaining: codeRemaining,
		WorkRemaining: workRemaining,
		ObservedAt:    now.UTC(),
	})
}

// TraeCNShouldUseWorkPool 判断这次推理请求是否走 Work 端点（access_type=1）。
//
// auto（默认）只在明确知道 IDE 池已经见底、Work 池还有额度时才切换：余额快照过期
// 就回到 IDE 池，避免上游补满额度后还继续扣 Work 积分。
func (a *Account) TraeCNShouldUseWorkPool() bool {
	return TraeCNShouldUseWorkPool(a.TraeCNCreditsPoolMode(), a.TraeCNCreditsBalance(), time.Now())
}

// TraeCNShouldUseWorkPool 是上面的纯函数版本，便于测试与跨实例复用。
func TraeCNShouldUseWorkPool(mode string, balance TraeCNCreditsBalance, now time.Time) bool {
	switch NormalizeTraeCNCreditsPoolMode(mode) {
	case TraeCNCreditsPoolWork:
		return true
	case TraeCNCreditsPoolCode:
		return false
	default:
		if !balance.Known(now) {
			return false
		}
		return balance.CodeRemaining <= 0 && balance.WorkRemaining > 0
	}
}
