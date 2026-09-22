package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexHarvestResponse struct {
	Status    int
	State     string
	Cookies   codexTicketCookies
	Terminal  bool
	Completed bool
}

// 生产采票使用独立 WebSocket，永不进入业务连接池。HTTP 分支供兼容性测试使用。
func performCodexHarvestTransport(req *http.Request, body []byte, client *http.Client) (codexHarvestResponse, error) {
	if codexTicketProbeURLForTest != "" && strings.HasPrefix(codexTicketProbeURLForTest, "http") {
		resp, err := client.Do(req)
		if err != nil {
			return codexHarvestResponse{}, err
		}
		defer resp.Body.Close()
		out := codexHarvestResponse{Status: resp.StatusCode, State: observedCodexTurnState(resp.Header.Get(codexTurnStateHeader)), Cookies: captureCodexTicketCookies(req.URL, codexTicketCookies{}, resp)}
		if resp.StatusCode == http.StatusOK {
			out.Terminal, out.Completed = validateCodexHarvestStream(resp.Body)
		}
		return out, nil
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return codexHarvestResponse{}, fmt.Errorf("采票代理传输类型不支持 WebSocket")
	}
	dialer := websocket.Dialer{Proxy: transport.Proxy, NetDialContext: transport.DialContext, TLSClientConfig: transport.TLSClientConfig, HandshakeTimeout: 25 * time.Second, EnableCompression: true}
	endpoint := *req.URL
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	}
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	}
	headers := req.Header.Clone()
	headers.Del("Accept")
	headers.Del("Content-Type")
	headers.Del("Content-Length")
	headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	headers.Del(codexTurnStateHeader)
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
	conn.SetReadLimit(4 << 20)
	cookieURL := *req.URL
	if cookieURL.Scheme == "wss" {
		cookieURL.Scheme = "https"
	}
	if cookieURL.Scheme == "ws" {
		cookieURL.Scheme = "http"
	}
	out := codexHarvestResponse{Status: http.StatusOK, State: observedCodexTurnState(resp.Header.Get(codexTurnStateHeader)), Cookies: captureCodexTicketCookies(&cookieURL, codexTicketCookies{}, resp)}
	body, _ = sjson.SetBytes(body, "type", "response.create")
	body, _ = sjson.DeleteBytes(body, "stream")
	if err = conn.WriteMessage(websocket.TextMessage, body); err != nil {
		return out, fmt.Errorf("WebSocket 采票发送失败")
	}
	for {
		_, frame, readErr := conn.ReadMessage()
		if readErr != nil {
			return out, fmt.Errorf("WebSocket 采票未完成")
		}
		if state := codexTurnStateFromFrame(frame); state != "" {
			out.State = state
		}
		switch gjson.GetBytes(frame, "type").String() {
		case "response.completed", "response.incomplete":
			out.Terminal = true
			out.Completed = true
			return out, nil
		case "response.failed", "error":
			out.Terminal = true
			return out, nil
		}
	}
}
