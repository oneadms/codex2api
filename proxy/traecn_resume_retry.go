package proxy

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const traeCNResumeTaskKey = "traecn_resume_task"

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

func (t *traeCNResumeTask) canRetryLocked() bool {
	return t.retrySafe && !t.generationStarted && !t.toolSubmitted &&
		(t.failure == "" || t.failure == "resume_task_expired") &&
		(t.status >= http.StatusBadRequest || t.terminalEvent == "response.failed")
}
