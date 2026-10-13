package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexHarvestMaxResponse = 1 << 20

type codexHarvestResponse struct {
	Status    int
	State     string
	Text      string
	Model     string
	Cookies   codexTicketCookies
	Terminal  bool
	Completed bool
}

// 两次探测都使用独立连接，保持同一出口、会话和协议，不进入业务连接池。
func performCodexHarvestTransport(req *http.Request, body []byte, client *http.Client, protocol string, seed codexTicketCookies) (codexHarvestResponse, error) {
	if protocol != "websocket" && req.URL.Scheme != "ws" && req.URL.Scheme != "wss" {
		req.Close = true
		resp, err := client.Do(req)
		if err != nil {
			return codexHarvestResponse{}, err
		}
		defer resp.Body.Close()
		out := codexHarvestResponse{Status: resp.StatusCode, State: observedCodexTurnState(resp.Header.Get(codexTurnStateHeader))}
		if resp.StatusCode != http.StatusOK {
			return out, nil
		}
		out.Cookies = harvestResponseCookies(req, resp, seed)
		raw, err := io.ReadAll(io.LimitReader(resp.Body, codexHarvestMaxResponse+1))
		if err != nil || len(raw) > codexHarvestMaxResponse {
			return out, fmt.Errorf("采票响应截断或超过大小限制")
		}
		if err = consumeCodexHarvestSSE(raw, &out); err != nil {
			return out, err
		}
		return out, nil
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return codexHarvestResponse{}, fmt.Errorf("采票代理不支持 WebSocket")
	}
	dialer := websocket.Dialer{Proxy: transport.Proxy, NetDialContext: transport.DialContext, TLSClientConfig: transport.TLSClientConfig, HandshakeTimeout: 25 * time.Second}
	endpoint := *req.URL
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	}
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	}
	headers := req.Header.Clone()
	if req.Host != "" {
		headers.Set("Host", req.Host)
	}
	headers.Del("Accept")
	headers.Del("Content-Type")
	headers.Del("Content-Length")
	headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	conn, resp, err := dialer.DialContext(req.Context(), endpoint.String(), headers)
	if err != nil {
		if resp != nil {
			if resp.Body != nil {
				resp.Body.Close()
			}
			return codexHarvestResponse{Status: resp.StatusCode}, nil
		}
		return codexHarvestResponse{}, fmt.Errorf("WebSocket 采票连接失败")
	}
	defer conn.Close()
	stop := context.AfterFunc(req.Context(), func() { conn.Close() })
	defer stop()
	if deadline, exists := req.Context().Deadline(); exists {
		conn.SetReadDeadline(deadline)
		conn.SetWriteDeadline(deadline)
	}
	conn.SetReadLimit(codexHarvestMaxResponse)
	out := codexHarvestResponse{Status: http.StatusOK, State: observedCodexTurnState(resp.Header.Get(codexTurnStateHeader)), Cookies: harvestResponseCookies(req, resp, seed)}
	body, _ = sjson.SetBytes(body, "type", "response.create")
	body, _ = sjson.DeleteBytes(body, "stream")
	if state := headers.Get(codexTurnStateHeader); state != "" {
		body, _ = sjson.SetBytes(body, "client_metadata."+codexTurnStateMetadataKey, state)
	}
	if err = conn.WriteMessage(websocket.TextMessage, body); err != nil {
		return out, fmt.Errorf("WebSocket 采票发送失败")
	}
	for total := 0; total <= codexHarvestMaxResponse; {
		kind, frame, err := conn.ReadMessage()
		if err != nil {
			return out, fmt.Errorf("WebSocket 采票未完成")
		}
		total += len(frame)
		if kind != websocket.TextMessage || total > codexHarvestMaxResponse {
			return out, fmt.Errorf("WebSocket 采票响应超出限制")
		}
		if err = consumeCodexHarvestEvent(frame, &out); err != nil {
			return out, err
		}
		if out.Terminal {
			return out, nil
		}
	}
	return out, fmt.Errorf("WebSocket 采票响应超出限制")
}

func harvestResponseCookies(req *http.Request, resp *http.Response, seed codexTicketCookies) codexTicketCookies {
	endpoint := *req.URL
	if req.Host != "" {
		endpoint.Host = req.Host
	}
	if endpoint.Scheme == "ws" {
		endpoint.Scheme = "http"
	}
	if endpoint.Scheme == "wss" {
		endpoint.Scheme = "https"
	}
	if auth.ConfiguredCodexTicketSettings().TargetLength != 780 {
		return captureCodexTicketCookies(&endpoint, seed, resp)
	}
	updated := false
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "__cflb" || cookie.Name == "__oailb" {
			updated = true
		}
	}
	// 路由更新必须成对，不能拼接新旧半对；完全未更新时才沿用本次候选。
	if updated {
		seed = codexTicketCookies{}
	}
	cookies := captureCodexTicketCookies(&endpoint, seed, resp)
	header, expiry, valid := auth.CodexRouteCookie(cookies.Header, time.Now())
	if !valid {
		return codexTicketCookies{}
	}
	cookies.Header = header
	if cookies.ExpiresAt.IsZero() || expiry.Before(cookies.ExpiresAt) {
		cookies.ExpiresAt = expiry
	}
	return cookies
}

func consumeCodexHarvestEvent(frame []byte, out *codexHarvestResponse) error {
	if !gjson.ValidBytes(frame) {
		return fmt.Errorf("采票事件格式错误")
	}
	event := gjson.ParseBytes(frame)
	if model := event.Get("response.model").String(); model != "" {
		if out.Model != "" && out.Model != model {
			return fmt.Errorf("采票响应模型发生变化")
		}
		out.Model = model
	}
	if state := codexTurnStateFromFrame(frame); state != "" {
		out.State = state
	}
	if event.Get("type").String() == "codex.response.metadata" {
		if state := observedCodexTurnState(event.Get("headers.x-codex-turn-state").String()); state != "" {
			out.State = state
		}
	}
	switch event.Get("type").String() {
	case "response.output_text.delta":
		out.Text += event.Get("delta").String()
	case "response.completed":
		out.Terminal, out.Completed = true, true
	case "response.failed", "response.incomplete", "error":
		out.Terminal, out.Completed = true, false
		status := int(event.Get("status").Int())
		if status == 0 {
			status = int(event.Get("error.status").Int())
		}
		code := event.Get("error.code").String()
		if code == "rate_limit_exceeded" || code == "usage_limit_reached" {
			status = 429
		}
		if status >= 400 && status <= 599 {
			out.Status = status
		}
		return fmt.Errorf("采票流返回失败或未完成事件")
	}
	return nil
}

// SSE 必须读完整个有界响应；完成事件后的错误和截断不能被提前返回掩盖。
func consumeCodexHarvestSSE(raw []byte, out *codexHarvestResponse) error {
	data := strings.TrimPrefix(string(raw), "\ufeff")
	data = strings.ReplaceAll(strings.ReplaceAll(data, "\r\n", "\n"), "\r", "\n")
	var payload []string
	flush := func() error {
		if len(payload) == 0 {
			return nil
		}
		frame := strings.Join(payload, "\n")
		payload = nil
		if frame == "[DONE]" {
			return nil
		}
		return consumeCodexHarvestEvent([]byte(frame), out)
	}
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			payload = append(payload, strings.TrimPrefix(value, " "))
		}
	}
	if len(payload) > 0 {
		return fmt.Errorf("采票 SSE 最后事件未完整结束")
	}
	if !out.Completed {
		return fmt.Errorf("采票流缺少完成事件")
	}
	return nil
}
