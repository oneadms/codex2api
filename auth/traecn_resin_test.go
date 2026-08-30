package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Cover the complete Store refresh path rather than only the exported token
// helper: the Store is responsible for supplying the stable DB account id that
// binds ExchangeToken and inference to the same Resin lease.
func TestTraeCNRefreshRoutesExchangeTokenThroughResin(t *testing.T) {
	previousDecorator := ResinRequestDecorator
	t.Cleanup(func() { ResinRequestDecorator = previousDecorator })

	var originCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"direct-at","refreshToken":"direct-rt","expiresIn":3600}`))
	}))
	defer origin.Close()

	type capturedRequest struct {
		path         string
		resinAccount string
		body         map[string]string
	}
	captured := make(chan capturedRequest, 1)
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode ExchangeToken body: %v", err)
		}
		captured <- capturedRequest{
			path:         r.URL.Path,
			resinAccount: r.Header.Get("X-Resin-Account"),
			body:         body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"new-at","refreshToken":"new-rt","expiresIn":3600}`))
	}))
	defer resin.Close()

	var decoratedTarget, decoratedAccount string
	ResinRequestDecorator = func(targetURL, accountID string) string {
		decoratedTarget = targetURL
		decoratedAccount = accountID
		return resin.URL + "/resin/exchange"
	}
	store := &Store{maxConcurrency: 1}
	account := &Account{
		DBID:         92001,
		UpstreamType: UpstreamTraeCN,
		AccessToken:  "old-at",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(-time.Minute),
		TraeCNHost:   origin.URL,
		// Resin must take precedence over an account-level forward proxy.
		ProxyURL: "http://127.0.0.1:1",
	}
	if err := store.refreshTraeCNAccount(context.Background(), account, true); err != nil {
		t.Fatalf("refreshTraeCNAccount() error = %v", err)
	}

	var request capturedRequest
	select {
	case request = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("ExchangeToken did not reach Resin")
	}
	if decoratedTarget != origin.URL+TraeCNExchangePath {
		t.Fatalf("decorated ExchangeToken target = %q", decoratedTarget)
	}
	if decoratedAccount != "92001" {
		t.Fatalf("decorator account = %q, want 92001", decoratedAccount)
	}
	if request.path != "/resin/exchange" {
		t.Fatalf("Resin request path = %q, want /resin/exchange", request.path)
	}
	if request.resinAccount != "92001" {
		t.Fatalf("X-Resin-Account = %q, want 92001", request.resinAccount)
	}
	if request.body["RefreshToken"] != "old-rt" || request.body["ClientID"] != TraeCNOAuthClientID {
		t.Fatalf("unexpected ExchangeToken body: %#v", request.body)
	}
	if got := originCalls.Load(); got != 0 {
		t.Fatalf("ExchangeToken origin calls = %d, want 0 while Resin is enabled", got)
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.AccessToken != "new-at" || account.RefreshToken != "new-rt" {
		t.Fatalf("refreshed credentials = %q/%q, want new-at/new-rt", account.AccessToken, account.RefreshToken)
	}
}

func TestTraeCNRefreshWithoutResinUsesDirectExchangeTokenRoute(t *testing.T) {
	previousDecorator := ResinRequestDecorator
	ResinRequestDecorator = nil
	t.Cleanup(func() { ResinRequestDecorator = previousDecorator })

	type capturedRequest struct {
		path         string
		resinAccount string
	}
	captured := make(chan capturedRequest, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{path: r.URL.Path, resinAccount: r.Header.Get("X-Resin-Account")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"direct-at","refreshToken":"direct-rt","expiresIn":3600}`))
	}))
	defer origin.Close()

	store := &Store{maxConcurrency: 1}
	account := &Account{
		DBID:         92002,
		UpstreamType: UpstreamTraeCN,
		AccessToken:  "old-at",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(-time.Minute),
		TraeCNHost:   origin.URL,
	}
	if err := store.refreshTraeCNAccount(context.Background(), account, true); err != nil {
		t.Fatalf("refreshTraeCNAccount() error = %v", err)
	}

	var request capturedRequest
	select {
	case request = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("ExchangeToken did not reach the direct origin")
	}
	if request.path != TraeCNExchangePath {
		t.Fatalf("direct ExchangeToken path = %q, want %q", request.path, TraeCNExchangePath)
	}
	if request.resinAccount != "" {
		t.Fatalf("direct ExchangeToken unexpectedly carries X-Resin-Account=%q", request.resinAccount)
	}
}

func TestTraeCNTransientRefreshDoesNotClaimZeroResinLease(t *testing.T) {
	previousDecorator := ResinRequestDecorator
	t.Cleanup(func() { ResinRequestDecorator = previousDecorator })

	var decoratorCalls atomic.Int32
	ResinRequestDecorator = func(targetURL, accountID string) string {
		decoratorCalls.Add(1)
		return targetURL
	}
	type capturedRequest struct {
		path         string
		resinAccount string
	}
	captured := make(chan capturedRequest, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{path: r.URL.Path, resinAccount: r.Header.Get("X-Resin-Account")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"transient-at","refreshToken":"transient-rt","expiresIn":3600}`))
	}))
	defer origin.Close()

	account := &Account{
		DBID:         0,
		UpstreamType: UpstreamTraeCN,
		RefreshToken: "old-rt",
		TraeCNHost:   origin.URL,
	}
	if err := account.EnsureTraeCNAccessToken(context.Background(), "", true); err != nil {
		t.Fatalf("EnsureTraeCNAccessToken() error = %v", err)
	}

	var request capturedRequest
	select {
	case request = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("transient ExchangeToken did not reach the provider origin")
	}
	if request.path != TraeCNExchangePath || request.resinAccount != "" {
		t.Fatalf("transient ExchangeToken path/header = %q/%q, want direct path and no Resin identity", request.path, request.resinAccount)
	}
	if got := decoratorCalls.Load(); got != 0 {
		t.Fatalf("Resin decorator calls = %d, want 0 for transient account", got)
	}
}
