package proxy

import (
	"bytes"
	"io"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Trae CN 的两个积分池（IDE/Code 与 Work）由请求体里的 access_type 选择：
// 实测 access_type=1 扣 Work 池，缺省（等价 0）扣 IDE 池。auto 模式下只有明确了
// “IDE 池见底、Work 池还有额度” 才会切到 Work，避免上游补额度后继续扣错池。

// traeCNAccessTypeForAccount 返回本次请求要用的 access_type。
func traeCNAccessTypeForAccount(account *auth.Account) int {
	if account == nil || !account.IsTraeCNAPI() {
		return 0
	}
	if account.TraeCNShouldUseWorkPool() {
		return auth.TraeCNWorkAccessType
	}
	return 0
}

// applyTraeCNAccessType 把 access_type 写进请求体；0 表示不写，保持抓包同款的
// 默认请求形状。
func applyTraeCNAccessType(body []byte, accessType int) []byte {
	if accessType == 0 || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	updated, err := sjson.SetBytes(body, "access_type", accessType)
	if err != nil {
		return body
	}
	return updated
}

// traeCNCreditsRemainKey 是上游用量通知里的分池余额字段：
// {"cn_credits_remain_info":{"ide_credits":0,"work_credits":2000}}。
const traeCNCreditsRemainKey = "cn_credits_remain_info"

// parseTraeCNCreditsRemain 从任意响应片段（SSE 文本或 JSON）里取分池余额。
// 只有两个字段都在且是数字时才认为有效，避免把半截数据写进账号。
func parseTraeCNCreditsRemain(raw []byte) (auth.TraeCNCreditsBalance, bool) {
	index := bytes.Index(raw, []byte(traeCNCreditsRemainKey))
	if index < 0 {
		return auth.TraeCNCreditsBalance{}, false
	}
	// 从字段名后第一个 '{' 开始解析，末尾多余的 SSE 文本由 gjson 忽略。
	start := bytes.IndexByte(raw[index:], '{')
	if start < 0 {
		return auth.TraeCNCreditsBalance{}, false
	}
	object := gjson.ParseBytes(raw[index+start:])
	if !object.IsObject() {
		return auth.TraeCNCreditsBalance{}, false
	}
	ide := object.Get("ide_credits")
	work := object.Get("work_credits")
	if ide.Type != gjson.Number || work.Type != gjson.Number {
		return auth.TraeCNCreditsBalance{}, false
	}
	return auth.TraeCNCreditsBalance{
		CodeRemaining: ide.Float(),
		WorkRemaining: work.Float(),
		ObservedAt:    time.Now().UTC(),
	}, true
}

// recordTraeCNCreditsRemainFromPayload 把响应里的分池余额记到账号上，供后续请求
// 选择积分池并决定多少冷却。返回是否成功记录。
func recordTraeCNCreditsRemainFromPayload(account *auth.Account, payload []byte) bool {
	if account == nil || len(payload) == 0 {
		return false
	}
	balance, ok := parseTraeCNCreditsRemain(payload)
	if !ok {
		return false
	}
	account.SetTraeCNCreditsBalance(balance)
	return true
}

// traeCNCreditsRemainScanner 是透传的响应包装：扫描上游 SSE 里的分池余额并更新
// 账号，不改变字节流本身。只在流的前 64 KiB 内扫描，避免长连接一直留状态。
type traeCNCreditsRemainScanner struct {
	io.ReadCloser
	account *auth.Account
	buffer  bytes.Buffer
	scanned bool
}

const traeCNCreditsRemainScanLimit = 64 << 10

func wrapTraeCNCreditsRemainScanner(body io.ReadCloser, account *auth.Account) io.ReadCloser {
	if body == nil || account == nil {
		return body
	}
	return &traeCNCreditsRemainScanner{ReadCloser: body, account: account}
}

func (s *traeCNCreditsRemainScanner) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if n > 0 && !s.scanned {
		s.buffer.Write(p[:n])
		// 字段可能在分片边界上被切断：解析成功才收工，否则继续攒到上限。
		if bytes.Contains(s.buffer.Bytes(), []byte(traeCNCreditsRemainKey)) &&
			recordTraeCNCreditsRemainFromPayload(s.account, s.buffer.Bytes()) {
			s.scanned = true
			s.buffer.Reset()
			return n, err
		}
		if s.buffer.Len() >= traeCNCreditsRemainScanLimit {
			s.scanned = true
			s.buffer.Reset()
		}
	}
	return n, err
}
