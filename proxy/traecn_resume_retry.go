package proxy

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const traeCNResumeTaskKey = "traecn_resume_task"

// 同一逻辑请求连续静默重开的次数上限，防止上游持续空输出或持续失败时无限重开。
// 用尽后不再注册续传任务，改为放弃续传、按普通流程服务。
const traeCNResumeSilentRestartLimit = 2

// 本次 HTTP 请求与后台任务的关系，只用于观测，不改变客户端行为。
const (
	traeCNResumeStatusHeader   = "X-Codex2API-Resume"
	traeCNResumeStatusAttached = "attached"
	traeCNResumeStatusFresh    = "fresh"
	traeCNResumeStatusRestart  = "restarted"
	traeCNResumeStatusBypass   = "bypass"
)

func traeCNResumeTaskRestarts(task *traeCNResumeTask) int {
	if task == nil {
		return 0
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.restarts
}

// 只有未派发请求或上游明确拒绝的尝试，才允许重新调度；网络错误不能证明未生成。
func markTraeCNResumeRetrySafe(c *gin.Context, safe bool) {
	value, _ := c.Get(traeCNResumeTaskKey)
	task, ok := value.(*traeCNResumeTask)
	if !ok {
		return
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	task.retrySafe = safe && !task.generationStarted
}

// canRetryLocked 判断客户端重发同一请求时能否直接重新调度，而不是把冲突交给客户端。
// 工具调用一旦交付给客户端就永远不重开，避免工具被重复执行。
func (t *traeCNResumeTask) canRetryLocked() bool {
	if t.toolSubmitted {
		return false
	}
	if t.retrySafe && !t.generationStarted &&
		(t.failure == "" || t.failure == "resume_task_expired") &&
		(t.status >= http.StatusBadRequest || t.terminalEvent == "response.failed") {
		return true
	}
	// 上游已给出失败终态：原任务不可能再恢复进度，重复接回只能回放失败。
	// 这类请求直接重新调度，账号池还有额度时客户端应当拿到新生成而不是冲突错误。
	if t.terminalEvent != "response.failed" || t.policyRefused {
		return false
	}
	return t.restarts < traeCNResumeSilentRestartLimit
}
