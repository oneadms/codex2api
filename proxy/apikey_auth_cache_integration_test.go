package proxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/redis/go-redis/v9"
)

func authIntegrationRedis(t testing.TB) cache.TokenCache {
	t.Helper()
	addr := os.Getenv("CODEX2API_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires isolated Redis in CODEX2API_TEST_REDIS_ADDR")
	}
	tc, err := cache.NewRedis(addr, "", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	return tc
}

func TestAPIKeyAuthCacheRedisAcrossInstances(t *testing.T) {
	backend := authIntegrationRedis(t)
	otherBackend := authIntegrationRedis(t)
	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	otherDB, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherDB.Close() })
	ctx := context.Background()
	key := "sk-real-redis-auth-cache"
	id, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "shared", Key: key, Limits: database.APIKeyLimits{ModelAllow: []string{"gpt-5.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	a := newAuthCacheTest(t, db, backend)
	b := newAuthCacheTest(t, otherDB, otherBackend)
	deadline := time.Now().Add(2 * time.Second)
	for a.invalidations.Load() < 2 || b.invalidations.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("subscribers did not connect")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok, err := a.resolve(ctx, key); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := b.resolve(ctx, key); err != nil || !ok || b.dbLoads.Load() != 0 || b.remoteHits.Load() != 1 {
		t.Fatal("second instance did not hit Redis")
	}
	before := b.invalidations.Load()
	if err := db.UpdateAPIKey(ctx, id, database.APIKeyUpdate{EnabledSet: true, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	(&Handler{authCache: a}).InvalidateAPIKeyAuthCache(ctx)
	deadline = time.Now().Add(time.Second)
	for b.invalidations.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("remote invalidation not received")
		}
		time.Sleep(time.Millisecond)
	}
	row, ok, err := b.resolve(ctx, key)
	if err != nil || !ok || row.Enabled {
		t.Fatal("remote instance admitted revoked key")
	}
}

func TestAPIKeyAuthCacheRedisTimeoutFallsBack(t *testing.T) {
	backend := authIntegrationRedis(t)
	db := newAuthCacheTestDB()
	a := newAuthCacheTest(t, db, backend)
	ctx := context.Background()
	if _, ok, err := a.resolve(ctx, db.row.Key); err != nil || !ok {
		t.Fatal(err)
	}
	control := redis.NewClient(&redis.Options{Addr: os.Getenv("CODEX2API_TEST_REDIS_ADDR")})
	defer control.Close()
	if err := control.Do(ctx, "CLIENT", "PAUSE", "2000", "ALL").Err(); err != nil {
		t.Fatal(err)
	}
	a.invalidate()
	started := time.Now()
	if _, ok, err := a.resolve(ctx, db.row.Key); err != nil || !ok {
		t.Fatalf("slow Redis prevented SQL fallback: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 1400*time.Millisecond {
		t.Fatalf("auth waited %s for paused Redis", elapsed)
	}
}

func BenchmarkAPIKeyAuthLookup(b *testing.B) {
	backend := authIntegrationRedis(b)
	db, err := database.New("sqlite", filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	key := "sk-auth-benchmark"
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{Name: "benchmark", Key: key, Limits: database.APIKeyLimits{ModelAllow: []string{"gpt-5.4"}}}); err != nil {
		b.Fatal(err)
	}
	legacy := &Handler{db: db, cache: backend}
	layered := &Handler{db: db, cfg: &config.Config{APIKeyAuthCacheEnabled: true}}
	layered.SetRuntimeCache(backend)
	b.Cleanup(layered.CloseAPIKeyAuthCache)
	for _, tc := range []struct {
		name    string
		handler *Handler
	}{{"previous", legacy}, {"l1_l2", layered}} {
		b.Run(tc.name, func(b *testing.B) {
			if _, ok, err := tc.handler.resolveAPIKey(key); err != nil || !ok {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, ok, err := tc.handler.resolveAPIKey(key); err != nil || !ok {
					b.Fatal(err)
				}
			}
		})
	}
}
