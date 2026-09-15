package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type batchCountingTokenCache struct {
	cache.TokenCache
	reads, batches, counterBatches atomic.Int64
	batchError                     bool
	readStarted, readRelease       chan struct{}
}

func (c *batchCountingTokenCache) SharedAcrossInstances() bool { return true }
func (c *batchCountingTokenCache) GetRuntime(ctx context.Context, ns, key string) (json.RawMessage, bool, error) {
	c.reads.Add(1)
	return c.TokenCache.GetRuntime(ctx, ns, key)
}
func (c *batchCountingTokenCache) GetRuntimeBatch(ctx context.Context, ns string, keys []string) (map[string]json.RawMessage, error) {
	c.batches.Add(1)
	if c.batchError {
		return nil, errors.New("unavailable batch")
	}
	return c.TokenCache.(cache.RuntimeBatchReader).GetRuntimeBatch(ctx, ns, keys)
}
func (c *batchCountingTokenCache) GetRuntimeCountersBatch(ctx context.Context, ns string, keys []string) (map[string]map[string]float64, error) {
	c.counterBatches.Add(1)
	if c.readStarted != nil {
		c.readStarted <- struct{}{}
	}
	if c.readRelease != nil {
		select {
		case <-c.readRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.TokenCache.(cache.RuntimeCounterBatchReader).GetRuntimeCountersBatch(ctx, ns, keys)
}

func limitTestContext(row *database.APIKeyRow) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set(contextAPIKeyRow, row)
	return c
}

func TestAPIKeyLimitsBatchPreservesOrderAndReducesReads(t *testing.T) {
	tc := &batchCountingTokenCache{TokenCache: cache.NewMemory(1)}
	t.Cleanup(func() { _ = tc.Close() })
	h := &Handler{cache: tc}
	row := &database.APIKeyRow{ID: 42, Limits: database.APIKeyLimits{RPM: 100, RPD: 100, CostLimitDaily: 10, CostLimit5h: 10, CostLimit7d: 10, CostLimit30d: 10}}
	for _, label := range []string{"rpm", "rpd"} {
		h.writeAPIKeyLimitCache(context.Background(), apiKeyLimitsCacheKey(row.ID, "req", label), &database.APIKeyWindowUsage{Requests: 1})
	}
	for _, label := range []string{"daily:" + database.StartOfDay(time.Now()).Format("2006-01-02"), "5h", "7d", "30d"} {
		usage := &database.APIKeyWindowUsage{UserBilled: 1}
		if label == "30d" {
			usage.UserBilled = 10
		}
		h.writeAPIKeyLimitCache(context.Background(), apiKeyLimitsCacheKey(row.ID, "usage", label), usage)
	}
	status, msg := h.enforceAPIKeyLimits(limitTestContext(row), "gpt-5.4")
	if status != 429 || !strings.Contains(msg, "30d") || tc.batches.Load() != 1 || tc.reads.Load() != 0 {
		t.Fatalf("batch result: %d %s; batches=%d reads=%d", status, msg, tc.batches.Load(), tc.reads.Load())
	}
	// An adapter that only implements TokenCache keeps the old sequential path.
	legacy := &Handler{cache: struct{ cache.TokenCache }{tc}}
	legacyStatus, legacyMsg := legacy.enforceAPIKeyLimits(limitTestContext(row), "gpt-5.4")
	if legacyStatus != status || legacyMsg != msg || tc.reads.Load() != 6 {
		t.Fatalf("legacy diverged: %d %s reads=%d", legacyStatus, legacyMsg, tc.reads.Load())
	}
	// Model rejection still precedes cache I/O.
	row.Limits.ModelAllow = []string{"different-model"}
	status, _ = h.enforceAPIKeyLimits(limitTestContext(row), "gpt-5.4")
	if status != 403 || tc.batches.Load() != 1 {
		t.Fatal("model rejection accessed Redis")
	}
}

func TestAPIKeyLimitsBatchMissCorruptionAndFailureUseSQL(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id, err := db.InsertAPIKey(ctx, "limited", "sk-batch-limits-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertUsageLog(ctx, &database.UsageLogInput{APIKeyID: id, AccountID: 1, Model: "gpt-5.4", Endpoint: "/v1/responses", StatusCode: 200, TotalTokens: 10}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	for _, scenario := range []string{"missing", "corrupt", "redis-error"} {
		t.Run(scenario, func(t *testing.T) {
			tc := &batchCountingTokenCache{TokenCache: cache.NewMemory(1), batchError: scenario == "redis-error"}
			t.Cleanup(func() { _ = tc.Close() })
			h := &Handler{db: db, cache: tc}
			h.writeAPIKeyLimitCache(ctx, apiKeyLimitsCacheKey(id, "req", "rpm"), &database.APIKeyWindowUsage{})
			if scenario == "corrupt" {
				if err := tc.SetRuntime(ctx, apiKeyLimitsCacheNamespace, apiKeyLimitsCacheKey(id, "req", "rpd"), json.RawMessage(`broken`), time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			row := &database.APIKeyRow{ID: id, Limits: database.APIKeyLimits{RPM: 100, RPD: 1}}
			status, msg := h.enforceAPIKeyLimits(limitTestContext(row), "gpt-5.4")
			if status != 429 || !strings.Contains(msg, "per day") || tc.batches.Load() != 1 || tc.reads.Load() != 0 {
				t.Fatalf("fallback result: %d %s; batches=%d reads=%d", status, msg, tc.batches.Load(), tc.reads.Load())
			}
		})
	}
}

func TestAPIKeyLimitsSingleWindowKeepsOneRead(t *testing.T) {
	tc := &batchCountingTokenCache{TokenCache: cache.NewMemory(1)}
	t.Cleanup(func() { _ = tc.Close() })
	h := &Handler{cache: tc}
	h.writeAPIKeyLimitCache(context.Background(), apiKeyLimitsCacheKey(7, "req", "rpm"), &database.APIKeyWindowUsage{Requests: 1})
	row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{RPM: 10}}
	if status, msg := h.enforceAPIKeyLimits(limitTestContext(row), "gpt-5.4"); status != 0 || tc.reads.Load() != 1 || tc.batches.Load() != 0 {
		t.Fatalf("single window: %d %s reads=%d batches=%d", status, msg, tc.reads.Load(), tc.batches.Load())
	}
}

func TestSharedScopeBatchSurvivesLeaderCancellation(t *testing.T) {
	tc := &batchCountingTokenCache{TokenCache: cache.NewMemory(1), readStarted: make(chan struct{}, 1), readRelease: make(chan struct{})}
	t.Cleanup(func() { _ = tc.Close() })
	ctx := context.Background()
	key := apiKeyScopeSharedDeltaKey(7, time.Now().Unix()/60)
	if err := tc.IncrRuntimeCounters(ctx, apiKeyScopeSharedDeltaNamespace, key, map[string]float64{"1:r": 2, "1:t": 20}, time.Minute); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cache: tc}
	leader, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { h.sharedScopeBuckets(leader, 7); close(done) }()
	select {
	case <-tc.readStarted:
	case <-time.After(time.Second):
		t.Fatal("read never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled leader kept waiting")
	}
	close(tc.readRelease)
	buckets, _ := h.sharedScopeBuckets(ctx, 7)
	var tokens int64
	for _, bucket := range buckets {
		tokens += bucket[1].Tokens
	}
	if tokens != 20 || tc.counterBatches.Load() != 1 {
		t.Fatalf("shared read: tokens=%d batches=%d", tokens, tc.counterBatches.Load())
	}
	h.sharedScopeBuckets(ctx, 7)
	if tc.counterBatches.Load() != 1 {
		t.Fatal("local snapshot not reused")
	}
}

type blockingDeltaCache struct {
	cache.TokenCache
	started, release, finished chan struct{}
}

func (c *blockingDeltaCache) SharedAcrossInstances() bool { return true }
func (c *blockingDeltaCache) IncrRuntimeCounters(ctx context.Context, ns, key string, values map[string]float64, ttl time.Duration) error {
	c.started <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := c.TokenCache.IncrRuntimeCounters(ctx, ns, key, values, ttl)
	c.finished <- struct{}{}
	return err
}

func TestSharedScopeWriterBackpressuresWithoutDropping(t *testing.T) {
	n := apiKeyScopeDeltaWriteSlots + 1
	tc := &blockingDeltaCache{TokenCache: cache.NewMemory(1), started: make(chan struct{}, n), release: make(chan struct{}), finished: make(chan struct{}, n)}
	t.Cleanup(func() { _ = tc.Close() })
	h := &Handler{cache: tc}
	for i := 0; i < n-1; i++ {
		h.publishSharedScopeDelta(7, 1, 10, 0.1)
	}
	for i := 0; i < n-1; i++ {
		select {
		case <-tc.started:
		case <-time.After(time.Second):
			t.Fatal("background write never started")
		}
	}
	done := make(chan struct{})
	go func() { h.publishSharedScopeDelta(7, 1, 10, 0.1); close(done) }()
	select {
	case <-tc.started:
	case <-time.After(time.Second):
		t.Fatal("fallback write never started")
	}
	select {
	case <-done:
		t.Fatal("saturated writer started another background job")
	default:
	}
	close(tc.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fallback write stuck")
	}
	for i := 0; i < n; i++ {
		select {
		case <-tc.finished:
		case <-time.After(time.Second):
			t.Fatal("write lost")
		}
	}
	var total float64
	for _, minute := range []int64{time.Now().Unix() / 60, time.Now().Unix()/60 - 1} {
		v, err := tc.GetRuntimeCounters(context.Background(), apiKeyScopeSharedDeltaNamespace, apiKeyScopeSharedDeltaKey(7, minute))
		if err != nil {
			t.Fatal(err)
		}
		total += v["1:r"]
	}
	if total != float64(n) {
		t.Fatalf("persisted %v of %d increments", total, n)
	}
}

type slowLookupCache struct {
	cache.TokenCache
	reads atomic.Int64
}

func (c *slowLookupCache) GetRuntime(ctx context.Context, ns, key string) (json.RawMessage, bool, error) {
	c.reads.Add(1)
	time.Sleep(30 * time.Millisecond)
	return c.TokenCache.GetRuntime(ctx, ns, key)
}

func TestAPIKeyLookupsShareOnlyOverlappingReads(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	var closeDB sync.Once
	t.Cleanup(func() { closeDB.Do(func() { _ = db.Close() }) })
	ctx := context.Background()
	id, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "limited", Key: "sk-coalesced-test", AllowedGroupIDs: []int64{3}, Limits: database.APIKeyLimits{ModelAllow: []string{"gpt-5.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	tc := &slowLookupCache{TokenCache: cache.NewMemory(1)}
	t.Cleanup(func() { _ = tc.Close() })
	h := &Handler{db: db, cache: tc}
	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	rows := make([]*database.APIKeyRow, workers)
	for i := range rows {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			row, ok, err := h.resolveAPIKey("sk-coalesced-test")
			if !ok || err != nil {
				t.Errorf("lookup: ok=%v err=%v", ok, err)
				return
			}
			rows[i] = row
		}(i)
	}
	close(start)
	wg.Wait()
	if tc.reads.Load() >= workers/2 {
		t.Fatalf("overlapping lookups were not coalesced: %d reads", tc.reads.Load())
	}
	for _, row := range rows {
		if row == nil {
			t.Fatal("missing result")
		}
	}
	rows[0].AllowedGroupIDs[0] = 99
	rows[0].Limits.ModelAllow[0] = "changed"
	for _, row := range rows[1:] {
		if row.AllowedGroupIDs[0] != 3 || row.Limits.ModelAllow[0] != "gpt-5.4" {
			t.Fatal("callers share mutable auth metadata")
		}
	}
	if err := db.UpdateAPIKey(ctx, id, database.APIKeyUpdate{EnabledSet: true, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	row, ok, err := h.resolveAPIKey("sk-coalesced-test")
	if err != nil || !ok || row.Enabled {
		t.Fatal("completed lookup hid revocation")
	}
	_, ok, err = h.resolveAPIKey("sk-new-after-miss")
	if err != nil || ok {
		t.Fatal("expected missing key")
	}
	if _, err := db.InsertAPIKey(ctx, "new", "sk-new-after-miss"); err != nil {
		t.Fatal(err)
	}
	_, ok, err = h.resolveAPIKey("sk-new-after-miss")
	if err != nil || !ok {
		t.Fatal("negative result was retained")
	}
	closeDB.Do(func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	})
	_, ok, err = h.resolveAPIKey("sk-coalesced-test")
	if ok || err == nil {
		t.Fatal("database failure misreported as auth miss")
	}
}
