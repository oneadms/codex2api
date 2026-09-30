package proxy

import (
	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/diag"
	"github.com/gin-gonic/gin"
)

func captureDiagnosticUpstream(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || !diag.Enabled() {
		return
	}
	api.SetDiagnosticUpstream(c, diag.UpstreamAttempt{
		Status: input.StatusCode, ErrorKind: input.UpstreamErrorKind,
		Message: input.ErrorMessage, RequestID: input.UpstreamRequestID,
		AccountID: input.AccountID, Attempt: input.AttemptIndex, Retry: input.IsRetryAttempt,
	})
}
