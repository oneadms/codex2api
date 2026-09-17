package proxy

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const debugAgentSessionID = "8b562e"

var (
	debugAgentLogOnce      sync.Once
	debugAgentLogPath      string
	debugAgentLogErrorOnce sync.Once
	debugAgentLogMu        sync.Mutex
)

// 诊断日志由服务端独立写入文件，开关默认关闭。
func debugAgentLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEBUG_AGENT_LOG"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func debugAgentLogPathResolved() string {
	debugAgentLogOnce.Do(func() {
		debugAgentLogPath = os.Getenv("DEBUG_AGENT_LOG_PATH")
		if debugAgentLogPath == "" {
			debugAgentLogPath = "debug-8b562e.log"
		}
	})
	return debugAgentLogPath
}

// 每行保存一条 JSON 记录；保留原有字段，便于按响应标识关联各阶段。
func debugAgentLog(location, message, hypothesisID, runID string, data map[string]any) {
	if !debugAgentLogEnabled() {
		return
	}
	payload := map[string]any{
		"sessionId":    debugAgentSessionID,
		"id":           "log_" + time.Now().Format("20060102150405.000"),
		"timestamp":    time.Now().UnixMilli(),
		"location":     location,
		"message":      message,
		"hypothesisId": hypothesisID,
		"runId":        runID,
		"data":         data,
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return
	}
	debugAgentLogMu.Lock()
	defer debugAgentLogMu.Unlock()
	f, err := os.OpenFile(debugAgentLogPathResolved(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		debugAgentLogWriteError(err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		debugAgentLogWriteError(err)
	}
}

// 路径或权限错误只告警一次，避免日志缺失时无从判断，也避免持续刷屏。
func debugAgentLogWriteError(err error) {
	debugAgentLogErrorOnce.Do(func() {
		log.Printf("[TRAECN-DEBUG] cannot write diagnostic log path=%q error=%q", debugAgentLogPathResolved(), err)
	})
}

func debugAgentTextTail(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[len(runes)-maxRunes:])
}
