package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newTraeCNModelSyncTestHandler(t *testing.T) (*Handler, func(string, string, bool) int64, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	calls := map[string]int{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Ide-Token")
		// Count the first (native-auth) request in each discovery attempt;
		// the compatibility fallbacks must also fail for the failing account.
		if r.URL.Path == "/api/ide/v1/batch_get_detail_param" && token != "" && r.Header.Get("Authorization") == "" {
			mu.Lock()
			calls[token]++
			mu.Unlock()
		}
		if token == "" {
			token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if strings.Contains(token, "fail") {
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/api/ide/v1/batch_get_detail_param" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"DeepSeek-V4-Pro","config_switch":true},{"config_name":"glm-5.2","config_switch":true}]}]}`))
	}))
	t.Cleanup(provider.Close)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	h := &Handler{db: db, store: store}
	insert := func(name, upstream string, load bool) int64 {
		t.Helper()
		id, err := db.InsertAccountWithUpstream(t.Context(), name, "trae", upstream, map[string]any{
			"upstream_type": upstream, "access_token": name,
			"expires_at":                              time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"traecn_host":                             provider.URL,
			auth.TraeCNModelAllowlistCredentialKey:    []string{"deepseek-v4-pro"},
			auth.TraeCNModelAllowlistSetCredentialKey: true,
			auth.TraeCNUpstreamModelsCredentialKey:    []string{"old-model", "DeepSeek-V4-Pro"},
			"models":                                  []string{"DeepSeek-V4-Pro"},
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		if load {
			if err := store.LoadAccountByID(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	return h, insert, func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[name]
	}
}

func TestTraeCNBatchModelSyncReportsEveryAccountAndKeepsFailedCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, insert, calls := newTraeCNModelSyncTestHandler(t)
	good := insert("good", auth.UpstreamTraeCN, true)
	lazy := insert("lazy", auth.UpstreamTraeCN, false)
	failed := insert("fail", auth.UpstreamTraeCN, true)
	other := insert("other", "codex", true)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/batch-sync-models?stream=true", strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d,%d,%d,%d,999999]}`, good, good, lazy, failed, other)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.BatchSyncTraeCNModels(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	var events []batchOperationEvent
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event batchOperationEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatalf("corrupt SSE frame %q: %v", line, err)
		}
		events = append(events, event)
	}
	if len(events) != 7 || events[0].Type != "start" || events[0].Total != 5 {
		t.Fatalf("unexpected events: %+v", events)
	}
	last := events[len(events)-1]
	if last.Type != "complete" || last.Success != 2 || last.Failed != 3 || last.Current != 5 || last.Action != traeCNBatchModelSyncAction {
		t.Fatalf("incorrect complete counts: %+v", last)
	}
	for i, event := range events[1 : len(events)-1] {
		if event.Type != "progress" || event.Current != i+1 || event.Success+event.Failed != int64(event.Current) || event.AccountID == 0 {
			t.Errorf("inconsistent serialized progress: %+v", event)
		}
		if event.Status == "failed" && event.Error == "" {
			t.Errorf("failed result omitted error: %+v", event)
		}
	}
	if calls("good") != 1 || calls("lazy") != 1 || calls("other") != 0 {
		t.Fatalf("duplicate or cross-channel request: good=%d lazy=%d other=%d", calls("good"), calls("lazy"), calls("other"))
	}
	for _, id := range []int64{good, lazy} {
		row, err := h.db.GetAccountByID(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if row.GetCredential(auth.TraeCNModelsSyncedAtCredentialKey) == "" || !slices.Contains(row.GetCredentialStringSlice(auth.TraeCNUpstreamModelsCredentialKey), "glm-5.2") {
			t.Fatalf("catalog not persisted for %d", id)
		}
		if models := h.store.FindByID(id).TraeCNEffectiveModels(); len(models) != 1 || !strings.EqualFold(models[0], "DeepSeek-V4-Pro") {
			t.Fatalf("allowlist lost: %v", models)
		}
	}
	row, err := h.db.GetAccountByID(t.Context(), failed)
	if err != nil || !slices.Contains(row.GetCredentialStringSlice(auth.TraeCNUpstreamModelsCredentialKey), "old-model") || row.GetCredential(auth.TraeCNModelsSyncedAtCredentialKey) != "" {
		t.Fatalf("failed sync replaced the old catalog: row=%+v err=%v", row, err)
	}
}

func TestTraeCNBatchModelSyncSelectorAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, insert, calls := newTraeCNModelSyncTestHandler(t)
	insert("selected", auth.UpstreamTraeCN, true)
	insert("excluded", auth.UpstreamTraeCN, true)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"selector":{"channel":"traecn","search":"selected"}}`, http.StatusOK},
		{`{"selector":{"channel":"codex"}}`, http.StatusBadRequest},
		{`{"ids":[1],"selector":{"channel":"traecn"}}`, http.StatusBadRequest},
		{`{"ids":[]}`, http.StatusBadRequest},
		{`{}`, http.StatusBadRequest},
		{`{"ids":"all"}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/traecn/batch-sync-models", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.BatchSyncTraeCNModels(c)
		if rec.Code != tc.status {
			t.Errorf("body=%s status=%d response=%s", tc.body, rec.Code, rec.Body.String())
		}
		if tc.status == http.StatusOK && !strings.Contains(rec.Body.String(), `"success":1`) {
			t.Fatalf("filtered batch counts: %s", rec.Body.String())
		}
	}
	if calls("selected") != 1 || calls("excluded") != 0 {
		t.Fatalf("filter ignored: selected=%d excluded=%d", calls("selected"), calls("excluded"))
	}
}

func TestTraeCNModelSyncWorkersBoundConcurrencyAndCancel(t *testing.T) {
	h := &Handler{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{}, traeCNModelSyncConcurrency)
	var active, maxActive, calls atomic.Int32
	var ids []int64
	for i := int64(1); i <= 1000; i++ {
		ids = append(ids, i)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.runTraeCNModelSyncTasks(ctx, ids, func(ctx context.Context, id int64) error {
			calls.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for old := maxActive.Load(); current > old && !maxActive.CompareAndSwap(old, current); old = maxActive.Load() {
			}
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}, nil)
	}()
	for i := 0; i < traeCNModelSyncConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled batch did not stop")
	}
	if maxActive.Load() != traeCNModelSyncConcurrency || calls.Load() != traeCNModelSyncConcurrency {
		t.Fatalf("unbounded or post-cancel work: max=%d calls=%d", maxActive.Load(), calls.Load())
	}
}
