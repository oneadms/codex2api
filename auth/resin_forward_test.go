package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// URL 未改写时也必须识别正向出口；代理拒绝后不能改走账号代理或直连。
func TestResinForwardRefreshUsesConnect(t *testing.T) {
	for _, session := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh-token", true: "session-token"}[session], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				wantHost := "auth.openai.com:443"
				if session {
					wantHost = "chatgpt.com:443"
				}
				wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("p2.90101:private-token"))
				if r.Method != http.MethodConnect || r.Host != wantHost || r.Header.Get("Proxy-Authorization") != wantAuth || r.Header.Get("X-Resin-Account") != "" {
					t.Error("incorrect auth CONNECT request")
				}
				w.WriteHeader(http.StatusProxyAuthRequired)
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			endpoint.User = url.UserPassword("p2.90101", "private-token")
			ctx := WithResinRequestResolver(t.Context(), func(_ context.Context, targetURL, accountID string) (ResinRoute, error) {
				return ResinRoute{Mode: ResinModeForward, URL: targetURL, AccountID: accountID, ProxyURL: endpoint.String()}, nil
			})
			var err error
			if session {
				_, _, err = RefreshWithSessionToken(ctx, "ST", "http://127.0.0.1:1", "90101")
			} else {
				_, _, err = RefreshAccessToken(ctx, "RT", "http://127.0.0.1:1", "90101")
			}
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
