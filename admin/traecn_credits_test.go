package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestTraeCNCreditsCacheRefreshAndFailure(t *testing.T) {
	var cache traeCNCreditsCache
	account := &auth.Account{DBID: 1}
	var calls int
	fail := false
	query := func(context.Context) (proxy.TraeCNCreditsSnapshot, error) {
		calls++
		if fail {
			return proxy.TraeCNCreditsSnapshot{}, errors.New("upstream unavailable")
		}
		return proxy.TraeCNCreditsSnapshot{Total: 1100, Used: 611.42, UpdatedAt: time.Now()}, nil
	}
	first, err := cache.get(t.Context(), account, false, query)
	if err != nil || first.Credits == nil || first.Stale {
		t.Fatalf("first = %+v, %v", first, err)
	}
	_, _ = cache.get(t.Context(), account, false, query)
	if calls != 1 {
		t.Fatalf("fresh cache made %d requests", calls)
	}
	fail = true
	stale, err := cache.get(t.Context(), account, true, query)
	if err != nil || !stale.Stale || stale.Error == "" || stale.Credits != first.Credits {
		t.Fatalf("lost previous balance: %+v, %v", stale, err)
	}
	_, _ = cache.get(t.Context(), account, false, query)
	if calls != 2 {
		t.Fatal("failed attempt was not cached")
	}
	fail = false
	cache.mu.Lock()
	entry := cache.entries[account]
	entry.ExpiresAt = time.Now().Add(-time.Second)
	cache.entries[account] = entry
	cache.mu.Unlock()
	recovered, err := cache.get(t.Context(), account, false, query)
	if err != nil || calls != 3 || recovered.Stale || recovered.Error != "" {
		t.Fatalf("expired cache not refreshed: %+v, %v", recovered, err)
	}
	// 同一 ID 被删除后重建，也不能沿用旧账号的缓存。
	_, _ = cache.get(t.Context(), &auth.Account{DBID: 1}, false, query)
	if calls != 4 {
		t.Fatal("replacement account reused old balance")
	}
}

func TestTraeCNCreditsCacheSingleflightAndCancellation(t *testing.T) {
	var cache traeCNCreditsCache
	account := &auth.Account{DBID: 1}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	query := func(ctx context.Context) (proxy.TraeCNCreditsSnapshot, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return proxy.TraeCNCreditsSnapshot{}, ctx.Err()
		}
		return proxy.TraeCNCreditsSnapshot{Total: 100}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := cache.get(ctx, account, false, query); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation = %v", err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := cache.get(t.Context(), account, false, query)
			if err != nil || result.Credits == nil || result.Credits.Total != 100 {
				t.Errorf("shared query lost: %+v, %v", result, err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestTraeCNCreditsCacheBound(t *testing.T) {
	var cache traeCNCreditsCache
	query := func(context.Context) (proxy.TraeCNCreditsSnapshot, error) {
		return proxy.TraeCNCreditsSnapshot{}, errors.New("no data")
	}
	for i := range traeCNCreditsCacheMaxEntries + 8 {
		response, err := cache.get(t.Context(), &auth.Account{DBID: int64(i + 1)}, false, query)
		if err != nil || response.Credits != nil || response.Stale || response.Error == "" {
			t.Fatalf("missing data became a balance: %+v, %v", response, err)
		}
	}
	if len(cache.entries) != traeCNCreditsCacheMaxEntries {
		t.Fatalf("cache grew to %d", len(cache.entries))
	}
}

func TestGetTraeCNCreditsServesSummaryWithoutCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 12, UpstreamType: auth.UpstreamTraeCN, AccessToken: "private-token"}
	store.AddAccount(account)
	handler := &Handler{store: store}
	_, err := handler.traeCNCredits.get(t.Context(), account, false, func(context.Context) (proxy.TraeCNCreditsSnapshot, error) {
		return proxy.TraeCNCreditsSnapshot{Total: 1100, Used: 611.42, Remaining: 488.58, UsedPercent: 55.58, UpdatedAt: time.Now()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id     string
		status int
	}{{"12", 200}, {"13", 404}, {"bad", 400}} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Params = gin.Params{{Key: "id", Value: tc.id}}
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/"+tc.id+"/traecn/credits", nil)
		handler.GetTraeCNCredits(c)
		if recorder.Code != tc.status || strings.Contains(recorder.Body.String(), "private-token") {
			t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
		}
		if tc.status == 200 && !strings.Contains(recorder.Body.String(), `"remaining":488.58`) {
			t.Fatalf("missing credits: %s", recorder.Body.String())
		}
	}
}
