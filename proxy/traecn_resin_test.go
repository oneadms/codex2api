package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func traeCNResinTestResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
}

// A configured Resin instance must own the Trae CN inference connection. In
// particular, merely adding the account identity header while still dialing
// the provider directly would defeat Resin's per-account egress lease.
func TestExecuteTraeCNRequestRoutesThroughResin(t *testing.T) {
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	var originCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originCalls.Add(1)
		traeCNResinTestResponse(w)
	}))
	defer origin.Close()

	type capturedRequest struct {
		method       string
		path         string
		resinAccount string
		authorize    string
		cloudToken   string
	}
	captured := make(chan capturedRequest, 1)
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{
			method:       r.Method,
			path:         r.URL.Path,
			resinAccount: r.Header.Get("X-Resin-Account"),
			authorize:    r.Header.Get("x-ide-token"),
			cloudToken:   r.Header.Get("x-ide-token"),
		}
		traeCNResinTestResponse(w)
	}))
	defer resin.Close()

	SetResinConfig(&ResinConfig{
		BaseURL:      resin.URL + "/lease-token",
		PlatformName: "traecn-test",
	})
	account := &auth.Account{
		DBID:         91001,
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "AT",
		RefreshToken: "RT",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   origin.URL,
		// Resin must take precedence over an account-level forward proxy.
		ProxyURL: "http://127.0.0.1:1",
		// Imported account headers must not select another Resin lease or
		// replace the provider credentials assembled by the gateway.
		CustomHeaders: map[string]string{
			"X-Resin-Account":  "wrong-account",
			"Authorization":    "Bearer wrong-token",
			"X-Cloudide-Token": "wrong-token",
		},
	}
	body := []byte(`{"model":"deepseek-v3","input":"hi","stream":true}`)
	resp, err := ExecuteTraeCNRequest(t.Context(), account, GrokProtocolResponses, body, body, "", nil)
	if err != nil {
		t.Fatalf("ExecuteTraeCNRequest() error = %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}

	var request capturedRequest
	select {
	case request = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("Trae CN inference did not reach Resin")
	}
	parsedOrigin, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/lease-token/traecn-test/http/" + parsedOrigin.Host + auth.TraeCNChatPath
	if request.method != http.MethodPost || request.path != wantPath {
		t.Fatalf("Resin request = %s %s, want POST %s", request.method, request.path, wantPath)
	}
	if request.resinAccount != "91001" {
		t.Fatalf("X-Resin-Account = %q, want 91001", request.resinAccount)
	}
	if request.authorize != "AT" {
		t.Fatalf("Trae auth header was lost through Resin: x-ide-token=%q", request.authorize)
	}
	if got := originCalls.Load(); got != 0 {
		t.Fatalf("provider origin calls = %d, want 0 while Resin is enabled", got)
	}
}

// With Resin disabled the existing direct/account-proxy path must remain
// byte-for-byte addressable and must not leak a Resin-only identity header.
func TestExecuteTraeCNRequestWithoutResinUsesDirectRoute(t *testing.T) {
	previousResin := resinCfg.Load()
	SetResinConfig(nil)
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	type capturedRequest struct {
		path         string
		resinAccount string
	}
	captured := make(chan capturedRequest, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{path: r.URL.Path, resinAccount: r.Header.Get("X-Resin-Account")}
		traeCNResinTestResponse(w)
	}))
	defer origin.Close()

	account := &auth.Account{
		DBID:         91002,
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "AT",
		RefreshToken: "RT",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   origin.URL,
	}
	body := []byte(`{"model":"deepseek-v3","input":"hi","stream":true}`)
	resp, err := ExecuteTraeCNRequest(t.Context(), account, GrokProtocolResponses, body, body, "", nil)
	if err != nil {
		t.Fatalf("ExecuteTraeCNRequest() error = %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}

	var request capturedRequest
	select {
	case request = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("Trae CN inference did not reach the direct origin")
	}
	if request.path != auth.TraeCNChatPath {
		t.Fatalf("direct request path = %q, want %q", request.path, auth.TraeCNChatPath)
	}
	if request.resinAccount != "" {
		t.Fatalf("direct request unexpectedly carries X-Resin-Account=%q", request.resinAccount)
	}
}

// Transient accounts have no durable identity with which Resin can associate
// a lease. Routing all of them as account "0" would collapse unrelated imports
// onto one sticky IP, so they retain the direct/account-proxy fallback.
func TestExecuteTraeCNRequestTransientAccountDoesNotClaimZeroResinLease(t *testing.T) {
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	var resinCalls atomic.Int32
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resinCalls.Add(1)
		traeCNResinTestResponse(w)
	}))
	defer resin.Close()
	SetResinConfig(&ResinConfig{BaseURL: resin.URL, PlatformName: "traecn-test"})

	type capturedRequest struct {
		path         string
		resinAccount string
	}
	captured := make(chan capturedRequest, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{path: r.URL.Path, resinAccount: r.Header.Get("X-Resin-Account")}
		traeCNResinTestResponse(w)
	}))
	defer origin.Close()

	account := &auth.Account{
		DBID:         0,
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "AT",
		RefreshToken: "RT",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   origin.URL,
	}
	body := []byte(`{"model":"deepseek-v3","input":"hi","stream":true}`)
	resp, err := ExecuteTraeCNRequest(t.Context(), account, GrokProtocolResponses, body, body, "", nil)
	if err != nil {
		t.Fatalf("ExecuteTraeCNRequest() error = %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}

	var request capturedRequest
	select {
	case request = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("transient Trae CN inference did not reach the provider origin")
	}
	if request.path != auth.TraeCNChatPath || request.resinAccount != "" {
		t.Fatalf("transient request path/header = %q/%q, want direct path and no Resin identity", request.path, request.resinAccount)
	}
	if got := resinCalls.Load(); got != 0 {
		t.Fatalf("Resin calls = %d, want 0 for transient account", got)
	}
}
