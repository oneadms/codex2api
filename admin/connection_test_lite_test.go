package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const liteProbeError = "{\"type\":\"error\",\"status\":400,\"error\":{\"code\":\"unsupported_value\",\"param\":\"reasoning.context\",\"type\":\"invalid_request_error\",\"message\":\"X-OpenAI-Internal-Codex-Responses-Lite requires `reasoning.context` to be `all_turns`.\"}}"

func TestConnectionTestLiteRetryPayload(t *testing.T) {
	payload := buildTestPayload("gpt-6-astra")
	if gjson.GetBytes(payload, "reasoning").Exists() {
		t.Fatal("the initial generic probe must not contain Codex-internal reasoning parameters")
	}
	retry := connectionTestLiteRetryPayload(payload, []byte(liteProbeError))
	if gjson.GetBytes(retry, "reasoning.context").String() != "all_turns" ||
		gjson.GetBytes(retry, "reasoning.effort").String() != "medium" ||
		gjson.GetBytes(retry, "parallel_tool_calls").Type != gjson.False {
		t.Fatalf("invalid Lite probe: %s", retry)
	}
	if gjson.GetBytes(payload, "reasoning").Exists() {
		t.Fatal("original payload was mutated")
	}
	if connectionTestLiteRetryPayload(retry, []byte(liteProbeError)) != nil {
		t.Fatal("already compatible probe must not be retried again")
	}
	explicit := []byte(`{"model":"gpt-6-astra","input":"hello","reasoning":{"effort":"high","summary":"auto"}}`)
	retry = connectionTestLiteRetryPayload(explicit, []byte(liteProbeError))
	if gjson.GetBytes(retry, "reasoning.effort").String() != "high" || gjson.GetBytes(retry, "reasoning.summary").String() != "auto" {
		t.Fatalf("explicit reasoning settings changed: %s", retry)
	}
	for _, data := range []string{
		`{"error":{"code":"unsupported_value","param":"reasoning.context","message":"invalid context"}}`,
		strings.Replace(liteProbeError, `"param":"reasoning.context"`, `"param":"model"`, 1),
		strings.Replace(liteProbeError, `"status":400`, `"status":429`, 1),
		strings.Replace(liteProbeError, `"code":"unsupported_value"`, `"code":"server_error"`, 1),
	} {
		if connectionTestLiteRetryPayload(payload, []byte(data)) != nil {
			t.Fatalf("unrelated error triggered Lite repair: %s", data)
		}
	}
}

type trackedProbeBody struct {
	io.Reader
	closed bool
}

func (b *trackedProbeBody) Close() error { b.closed = true; return nil }

type failedProbeReader struct{ err error }

func (r failedProbeReader) Read([]byte) (int, error) { return 0, r.err }

func TestPeekConnectionTestErrorPreservesResponse(t *testing.T) {
	for name, raw := range map[string]string{
		"success":            antigravityTestSSEBody("OK"),
		"created_then_error": "data: {\"type\":\"response.created\"}\n\ndata: " + liteProbeError + "\n\n",
		"large_event":        "data: " + strings.Repeat("x", 128*1024) + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := &trackedProbeBody{Reader: strings.NewReader(raw)}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
			data := peekConnectionTestError(resp)
			if connectionTestLiteRetryPayload(buildTestPayload("gpt-6-astra"), data) != nil {
				t.Fatal("a stream that has started must not be retried")
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil || string(got) != raw {
				t.Fatalf("response bytes lost: got %d bytes, want %d; error=%v", len(got), len(raw), err)
			}
			resp.Body.Close()
			if !body.closed {
				t.Fatal("original body was not closed")
			}
		})
	}
	readErr := errors.New("upstream read failure")
	body := &trackedProbeBody{Reader: io.MultiReader(strings.NewReader("partial"), failedProbeReader{readErr})}
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: body}
	if peekConnectionTestError(resp) != nil {
		t.Fatal("failed read must not trigger repair")
	}
	got, err := io.ReadAll(resp.Body)
	if string(got) != "partial" || !errors.Is(err, readErr) {
		t.Fatalf("read failure was not replayed: %q, %v", got, err)
	}
}

func TestConnectionProbeRecoversFromLiteValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, format := range []string{"json", "sse"} {
		for _, mode := range []string{"single", "batch", "recycle"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					if requests.Add(1) == 1 {
						if gjson.GetBytes(body, "reasoning").Exists() {
							t.Errorf("first probe unexpectedly changed: %s", body)
						}
						if format == "json" {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							io.WriteString(w, liteProbeError)
						} else {
							w.Header().Set("Content-Type", "text/event-stream")
							io.WriteString(w, "event: error\ndata: "+liteProbeError+"\n\n")
						}
						return
					}
					if gjson.GetBytes(body, "reasoning.context").String() != "all_turns" ||
						gjson.GetBytes(body, "reasoning.effort").String() != "medium" ||
						gjson.GetBytes(body, "parallel_tool_calls").Type != gjson.False {
						t.Errorf("retry is not compatible with unchanged vm2api conversion: %s", body)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, antigravityTestSSEBody("OK"))
				}))
				defer server.Close()
				store := auth.NewStore(nil, nil, nil)
				account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses,
					BaseURL: server.URL, APIKey: "test-key", Models: []string{"gpt-6-astra"}, Status: auth.StatusReady}
				store.AddAccount(account)
				h := &Handler{store: store}
				if mode == "single" {
					router := gin.New()
					router.GET("/accounts/:id/test", h.TestConnection)
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/42/test", nil))
					if !strings.Contains(recorder.Body.String(), `"success":true`) || strings.Contains(recorder.Body.String(), "unsupported_value") {
						t.Fatalf("probe failed: %s", recorder.Body.String())
					}
				} else {
					run := h.runSingleBatchTest
					if mode == "recycle" {
						run = h.runRecycleBinSingleTest
					}
					if status, message := run(context.Background(), account); status != "success" {
						t.Fatalf("probe failed: %s %s", status, message)
					}
				}
				if got := requests.Load(); got != 2 {
					t.Fatalf("requests = %d, want 2", got)
				}
			})
		}
	}
}

func TestConnectionProbeLiteRetryIsBounded(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary_api", true: "repeated_rejection"}[reject], func(t *testing.T) {
			var requests atomic.Int32
			raw := antigravityTestSSEBody("OK")
			if reject {
				raw = "data: " + liteProbeError + "\n\n"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if !reject {
					body, _ := io.ReadAll(r.Body)
					if gjson.GetBytes(body, "reasoning").Exists() {
						t.Error("ordinary API received internal reasoning parameters")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, raw)
			}))
			defer server.Close()
			h := &Handler{store: auth.NewStore(nil, nil, nil)}
			account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "test-key"}
			resp, _, err := h.executeRelayConnectionTest(context.Background(), account, buildTestPayload("gpt-6-astra"), "", true)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil || string(got) != raw {
				t.Fatalf("final response changed: %s, %v", got, err)
			}
			want := int32(1)
			if reject {
				want = 2
			}
			if requests.Load() != want {
				t.Fatalf("requests = %d, want %d", requests.Load(), want)
			}
		})
	}
}
