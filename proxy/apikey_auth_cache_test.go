package proxy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type authCacheTestDB struct {
	mu                       sync.Mutex
	state                    database.APIKeyAuthRevision
	row                      *database.APIKeyRow
	err                      error
	loads, quotaReads        int
	readStarted, readRelease chan struct{}
}

func newAuthCacheTestDB() *authCacheTestDB {
	return &authCacheTestDB{state: database.APIKeyAuthRevision{Namespace: "auth-test", Generation: 1, KeyCount: 1}, row: &database.APIKeyRow{ID: 7, Key: "sk-auth-cache-test", Name: "test", Enabled: true, AllowedGroupIDs: []int64{3}, Limits: database.APIKeyLimits{ModelAllow: []string{"gpt-5.4"}}}}
}
func (d *authCacheTestDB) GetAPIKeyAuthRevision(context.Context) (database.APIKeyAuthRevision, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state, d.err
}
func (d *authCacheTestDB) GetAPIKeyByValue(ctx context.Context, key string) (*database.APIKeyRow, error) {
	d.mu.Lock()
	d.loads++
	var row *database.APIKeyRow
	if d.row != nil && d.row.Key == key {
		row = cloneAPIKeyLookupRow(d.row)
	}
	err, started, release := d.err, d.readStarted, d.readRelease
	d.readStarted = nil
	d.readRelease = nil
	d.mu.Unlock()
	if started != nil {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, sql.ErrNoRows
	}
	return row, nil
}
func (d *authCacheTestDB) GetAPIKeyAuthQuota(context.Context, int64) (float64, database.APIKeyAuthRevision, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.quotaReads++
	if d.err != nil {
		return 0, d.state, d.err
	}
	if d.row == nil {
		return 0, d.state, sql.ErrNoRows
	}
	return d.row.QuotaUsed, d.state, nil
}

type authCacheTestBackend struct {
	cache.TokenCache
	reads, writes              atomic.Int64
	broken                     atomic.Bool
	writeStarted, writeRelease chan struct{}
}

func (c *authCacheTestBackend) SharedAcrossInstances() bool { return true }
func (c *authCacheTestBackend) GetRuntime(ctx context.Context, ns, key string) (json.RawMessage, bool, error) {
	c.reads.Add(1)
	if c.broken.Load() {
		return nil, false, errors.New("Redis unavailable")
	}
	return c.TokenCache.GetRuntime(ctx, ns, key)
}
func (c *authCacheTestBackend) SetRuntime(ctx context.Context, ns, key string, raw json.RawMessage, ttl time.Duration) error {
	n := c.writes.Add(1)
	if c.broken.Load() {
		return errors.New("Redis unavailable")
	}
	if n == 1 && c.writeStarted != nil {
		close(c.writeStarted)
		select {
		case <-c.writeRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.TokenCache.SetRuntime(ctx, ns, key, raw, ttl)
}

func newAuthCacheTest(t testing.TB, db apiKeyAuthStore, backend cache.TokenCache) *apiKeyAuthCache {
	t.Helper()
	a := newAPIKeyAuthCache(db, backend)
	t.Cleanup(a.close)
	return a
}

func expireAuthRevision(a *apiKeyAuthCache) {
	a.mu.Lock()
	a.checkedAt = time.Now().Add(-apiKeyAuthRevisionTTL - time.Second)
	a.mu.Unlock()
}

func TestAPIKeyAuthCacheLayersAndQuota(t *testing.T) {
	db := newAuthCacheTestDB()
	db.row.QuotaLimit = 10
	db.row.QuotaUsed = 1
	backend := &authCacheTestBackend{TokenCache: cache.NewMemory(1)}
	t.Cleanup(func() { _ = backend.Close() })
	a := newAuthCacheTest(t, db, backend)
	ctx := context.Background()
	key := db.row.Key
	row, ok, err := a.resolve(ctx, key)
	if err != nil || !ok || row.QuotaUsed != 1 {
		t.Fatalf("cold auth: %v %v", ok, err)
	}
	row.Limits.ModelAllow[0] = "tampered"
	row.AllowedGroupIDs[0] = 99
	db.mu.Lock()
	db.row.QuotaUsed = 10
	db.mu.Unlock()
	row, ok, err = a.resolve(ctx, key)
	if err != nil || !ok || !row.IsQuotaExhausted() || row.Limits.ModelAllow[0] != "gpt-5.4" || row.AllowedGroupIDs[0] != 3 {
		t.Fatalf("L1 clone/usage: %+v %v", row, err)
	}
	if db.loads != 1 || db.quotaReads != 2 || backend.reads.Load() != 1 {
		t.Fatalf("unexpected I/O: loads=%d quota=%d Redis=%d", db.loads, db.quotaReads, backend.reads.Load())
	}
	b := newAuthCacheTest(t, db, backend)
	row, ok, err = b.resolve(ctx, key)
	if err != nil || !ok || row.QuotaUsed != 10 || db.loads != 1 || b.remoteHits.Load() != 1 {
		t.Fatalf("L2 auth failed: %v %v", ok, err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	raw, _, err := backend.GetRuntime(ctx, apiKeyAuthNamespace, authSnapshotKey(db.state, digest))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("raw credential persisted in shared snapshot")
	}
	var record apiKeyAuthRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Row.QuotaUsed != 0 || record.Row.TotalUsed != 0 {
		t.Fatal("dynamic usage persisted in config cache")
	}
	// Simulate a missed Pub/Sub message: durable revision checking still revokes.
	db.mu.Lock()
	db.row.Enabled = false
	db.state.Generation++
	db.mu.Unlock()
	expireAuthRevision(b)
	row, ok, err = b.resolve(ctx, key)
	if err != nil || !ok || row.Enabled {
		t.Fatalf("missed invalidation kept key enabled: %v %v", ok, err)
	}
}

func TestAPIKeyAuthCacheDelayedSQLCannotRefill(t *testing.T) {
	db := newAuthCacheTestDB()
	started, release := make(chan struct{}), make(chan struct{})
	db.readStarted, db.readRelease = started, release
	a := newAuthCacheTest(t, db, cache.NewMemory(1))
	ctx := context.Background()
	done := make(chan *database.APIKeyRow, 1)
	go func() {
		row, _, err := a.resolve(ctx, "sk-auth-cache-test")
		if err != nil {
			t.Error(err)
		}
		done <- row
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("SQL read did not start")
	}
	db.mu.Lock()
	db.row.Enabled = false
	db.state.Generation++
	db.mu.Unlock()
	a.invalidate()
	close(release)
	select {
	case row := <-done:
		if row == nil || row.Enabled {
			t.Fatal("old SQL result survived invalidation")
		}
	case <-time.After(time.Second):
		t.Fatal("read did not finish")
	}
}

func TestAPIKeyAuthCacheDelayedRedisWriteUsesOldRevision(t *testing.T) {
	db := newAuthCacheTestDB()
	backend := &authCacheTestBackend{TokenCache: cache.NewMemory(1), writeStarted: make(chan struct{}), writeRelease: make(chan struct{})}
	a := newAuthCacheTest(t, db, backend)
	b := newAuthCacheTest(t, db, backend)
	done := make(chan *database.APIKeyRow, 1)
	go func() {
		row, _, err := a.resolve(context.Background(), "sk-auth-cache-test")
		if err != nil {
			t.Error(err)
		}
		done <- row
	}()
	select {
	case <-backend.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	db.mu.Lock()
	db.row.Enabled = false
	db.state.Generation++
	db.mu.Unlock()
	a.invalidate()
	row, _, err := b.resolve(context.Background(), "sk-auth-cache-test")
	if err != nil || row == nil || row.Enabled {
		t.Fatal("new instance read old revision")
	}
	close(backend.writeRelease)
	select {
	case row := <-done:
		if row == nil || row.Enabled {
			t.Fatal("delayed old Redis write revived credential")
		}
	case <-time.After(time.Second):
		t.Fatal("write did not finish")
	}
}

func TestAPIKeyAuthCacheFallbackIsolationAndNegativeBounds(t *testing.T) {
	db := newAuthCacheTestDB()
	backend := &authCacheTestBackend{TokenCache: cache.NewMemory(1)}
	a := newAuthCacheTest(t, db, backend)
	ctx := context.Background()
	backend.broken.Store(true)
	if _, ok, err := a.resolve(ctx, "sk-auth-cache-test"); err != nil || !ok {
		t.Fatal("Redis failure prevented SQL fallback")
	}
	backend.broken.Store(false)
	other := newAuthCacheTestDB()
	other.state.Namespace = "other-database"
	other.row.Enabled = false
	b := newAuthCacheTest(t, other, backend)
	row, ok, err := b.resolve(ctx, "sk-auth-cache-test")
	if err != nil || !ok || row.Enabled {
		t.Fatal("snapshot crossed database scope")
	}
	before := backend.writes.Load()
	for i := 0; i < apiKeyAuthMaxEntries+8; i++ {
		if _, ok, err := a.resolve(ctx, fmt.Sprintf("missing-%d", i)); err != nil || ok {
			t.Fatal("invalid credential admitted")
		}
	}
	if len(a.entries) > apiKeyAuthMaxEntries || a.bytes > apiKeyAuthMaxBytes || backend.writes.Load() != before {
		t.Fatal("negative cache escaped bounds or wrote Redis")
	}
	loads := db.loads
	if _, ok, err := a.resolve(ctx, "missing-4096"); err != nil || ok || db.loads != loads {
		t.Fatal("negative cache did not suppress repeated misses")
	}
	db.mu.Lock()
	db.err = errors.New("database unavailable")
	db.mu.Unlock()
	expireAuthRevision(a)
	if _, _, err := a.resolve(ctx, "sk-auth-cache-test"); err == nil {
		t.Fatal("failed revision validation served cached credential")
	}
}

func TestAPIKeyAuthCacheOversizeAndExpiry(t *testing.T) {
	db := newAuthCacheTestDB()
	db.row.Limits.ModelAllow = []string{strings.Repeat("x", apiKeyAuthMaxEntryBytes)}
	backend := &authCacheTestBackend{TokenCache: cache.NewMemory(1)}
	a := newAuthCacheTest(t, db, backend)
	if _, ok, err := a.resolve(context.Background(), db.row.Key); err != nil || !ok {
		t.Fatal(err)
	}
	if len(a.entries) != 0 || backend.writes.Load() != 0 || a.bypasses.Load() != 1 {
		t.Fatal("oversized snapshot cached")
	}
	db.row.Limits.ModelAllow = nil
	db.row.ExpiresAt = sql.NullTime{Valid: true, Time: time.Now().Add(-time.Second)}
	row, ok, err := a.resolve(context.Background(), db.row.Key)
	if err != nil || !ok || !row.IsExpired(time.Now()) {
		t.Fatal("credential expiry lost")
	}
}

func TestAPIKeyAuthCacheUnavailableNeverEnablesAnonymous(t *testing.T) {
	db := newAuthCacheTestDB()
	db.err = errors.New("database unavailable")
	h := &Handler{cfg: &config.Config{AllowAnonymousV1: true}, authCache: newAuthCacheTest(t, db, cache.NewMemory(1))}
	r := gin.New()
	r.Use(h.APIKeyAuthMiddleware())
	r.GET("/test", func(c *gin.Context) { c.Status(200) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))
	if w.Code != 503 {
		t.Fatalf("unavailable revision returned %d; anonymous bypass", w.Code)
	}
}

func TestAPIKeyAuthCacheCanceledLeaderDoesNotCancelFollowers(t *testing.T) {
	db := newAuthCacheTestDB()
	started, release := make(chan struct{}), make(chan struct{})
	db.readStarted, db.readRelease = started, release
	a := newAuthCacheTest(t, db, cache.NewMemory(1))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := a.resolve(ctx, "sk-auth-cache-test"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("lookup not started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled lookup still waiting")
	}
	close(release)
	if _, ok, err := a.resolve(context.Background(), "sk-auth-cache-test"); err != nil || !ok {
		t.Fatal("canceled leader poisoned shared lookup")
	}
	if db.loads != 1 {
		t.Fatalf("shared lookup repeated %d times", db.loads)
	}
}

func TestAPIKeyAuthCacheRejectsWrongOwnerAndVersion(t *testing.T) {
	db := newAuthCacheTestDB()
	backend := &authCacheTestBackend{TokenCache: cache.NewMemory(1)}
	key := db.row.Key
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	storeKey := authSnapshotKey(db.state, digest)
	for _, kind := range []string{"owner", "scope", "version", "expired", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			row := cloneAPIKeyLookupRow(db.row)
			row.Key = ""
			row.Enabled = false
			record := apiKeyAuthRecord{Version: 1, Generation: 1, Scope: db.state.Namespace, KeyDigest: digest, ExpiresAt: time.Now().Add(time.Minute), Row: row}
			switch kind {
			case "owner":
				record.KeyDigest = "another-key"
			case "scope":
				record.Scope = "another-database"
			case "version":
				record.Version = 2
			case "expired":
				record.ExpiresAt = time.Now().Add(-time.Second)
			}
			raw, _ := json.Marshal(record)
			if kind == "malformed" {
				raw = []byte(`bad-json`)
			}
			if err := backend.SetRuntime(context.Background(), apiKeyAuthNamespace, storeKey, raw, time.Minute); err != nil {
				t.Fatal(err)
			}
			a := newAuthCacheTest(t, db, backend)
			got, ok, err := a.resolve(context.Background(), key)
			if err != nil || !ok || !got.Enabled || a.dbLoads.Load() != 1 {
				t.Fatalf("invalid shared snapshot trusted: %v %v", ok, err)
			}
		})
	}
}

func TestAPIKeyAuthCacheCloseCancelsSharedWork(t *testing.T) {
	db := newAuthCacheTestDB()
	started := make(chan struct{})
	db.readStarted = started
	db.readRelease = make(chan struct{})
	a := newAuthCacheTest(t, db, cache.NewMemory(1))
	done := make(chan error, 1)
	go func() { _, _, err := a.resolve(context.Background(), "sk-auth-cache-test"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("lookup not started")
	}
	closed := make(chan struct{})
	go func() { a.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cache shutdown left lookup running")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown result: %v", err)
	}
}

func TestAPIKeyAuthCacheRejectableCredentialsSkipQuotaReads(t *testing.T) {
	db := newAuthCacheTestDB()
	db.row.QuotaLimit = 10
	db.row.Enabled = false
	a := newAuthCacheTest(t, db, cache.NewMemory(1))
	row, ok, err := a.resolve(context.Background(), db.row.Key)
	if err != nil || !ok || row.Enabled || db.quotaReads != 0 {
		t.Fatal("disabled key performed a quota lookup")
	}
	db.row.Enabled = true
	db.row.ExpiresAt = sql.NullTime{Valid: true, Time: time.Now().Add(-time.Second)}
	db.state.Generation++
	a.invalidate()
	row, ok, err = a.resolve(context.Background(), db.row.Key)
	if err != nil || !ok || !row.IsExpired(time.Now()) || db.quotaReads != 0 {
		t.Fatal("expired key performed a quota lookup")
	}
}
