package admin

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// executeRelayConnectionTest only adapts gateway-generated connection probes.
// A normal Responses API must not receive Codex-internal parameters based on
// its model name. Require the upstream's specific Lite rejection first.
func (h *Handler) executeRelayConnectionTest(ctx context.Context, account *auth.Account, payload []byte, proxyURL string, persistent bool) (*http.Response, []byte, error) {
	send := func(body []byte) (*http.Response, error) {
		if persistent {
			return proxy.ExecuteRelayStyleRequestWithStore(ctx, h.store, account, body, proxyURL, nil)
		}
		return proxy.ExecuteRelayStyleRequest(ctx, account, body, proxyURL, nil)
	}
	resp, err := send(payload)
	if err != nil || !account.IsOpenAIResponsesAPI() || ctx.Err() != nil {
		return resp, payload, err
	}
	retryBody := connectionTestLiteRetryPayload(payload, peekConnectionTestError(resp))
	if retryBody == nil || ctx.Err() != nil {
		return resp, payload, nil
	}
	_ = resp.Body.Close()
	resp, err = send(retryBody)
	return resp, retryBody, err
}

func connectionTestLiteRetryPayload(payload, data []byte) []byte {
	errorObject := gjson.GetBytes(data, "error")
	message := errorObject.Get("message").String()
	if errorObject.Get("code").String() != "unsupported_value" ||
		errorObject.Get("param").String() != "reasoning.context" ||
		!strings.Contains(message, "X-OpenAI-Internal-Codex-Responses-Lite") ||
		!strings.Contains(message, "all_turns") {
		return nil
	}
	if status := gjson.GetBytes(data, "status"); status.Exists() && status.Int() != http.StatusBadRequest {
		return nil
	}
	retryBody, err := sjson.SetBytes(payload, "reasoning.context", "all_turns")
	if err != nil {
		return nil
	}
	// vm2api drops the reasoning object when effort is absent; medium is its
	// existing default probe effort. Keep an explicitly selected effort intact.
	if strings.TrimSpace(gjson.GetBytes(retryBody, "reasoning.effort").String()) == "" {
		retryBody, err = sjson.SetBytes(retryBody, "reasoning.effort", "medium")
		if err != nil {
			return nil
		}
	}
	// Match the existing Codex Lite normalization in proxy/executor.go.
	retryBody, err = sjson.SetBytes(retryBody, "parallel_tool_calls", false)
	if err != nil || bytes.Equal(retryBody, payload) {
		return nil
	}
	return retryBody
}

// Inspect only an HTTP 400 body or the first SSE data event. Never retry after
// response.created/content or consume a successful stream. Replay every byte
// read, including read-ahead, and leave the original body responsible for Close.
func peekConnectionTestError(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	sse := resp.StatusCode == http.StatusOK && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if resp.StatusCode != http.StatusBadRequest && !sse {
		return nil
	}
	source := resp.Body
	var captured bytes.Buffer
	reader := io.TeeReader(io.LimitReader(source, 64*1024), &captured)
	var data []byte
	var err error
	if sse {
		err = proxy.ReadSSEStream(reader, func(event []byte) bool {
			data = bytes.Clone(event)
			return false
		})
	} else {
		data, err = io.ReadAll(reader)
	}
	resp.Body = &connectionTestReplayBody{prefix: bytes.NewReader(captured.Bytes()), source: source, readErr: err}
	if err != nil {
		return nil
	}
	return data
}

type connectionTestReplayBody struct {
	prefix  *bytes.Reader
	source  io.ReadCloser
	readErr error
}

func (b *connectionTestReplayBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	if b.readErr != nil {
		err := b.readErr
		b.readErr = nil
		return 0, err
	}
	return b.source.Read(p)
}

func (b *connectionTestReplayBody) Close() error { return b.source.Close() }
