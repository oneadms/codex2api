package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
)

func TestCodexHarvestPersistence(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "harvest.db")
			if driver == "postgres" {
				dsn = os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("未配置 PostgreSQL 测试库")
				}
			}
			db, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			repo := db.HarvestRepository()
			controls := harvest.NewCodexHarvestService(repo, repo, nil)
			settings := harvest.CodexHarvestControls{Version: 1, NodeMemoryEnabled: true, Speed: harvest.CodexHarvestSpeedPresets()["burst"]}
			if err = controls.SaveControls(ctx, settings); err != nil {
				t.Fatal(err)
			}
			reloaded := harvest.NewCodexHarvestService(repo, repo, nil)
			got, configured, err := reloaded.Controls(ctx)
			if err != nil || !configured || got != settings {
				t.Fatalf("控制设置未持久化: %v", err)
			}
			scope := harvest.CodexHarvestNodeScope{PoolID: "pool", AccountID: time.Now().UnixNano(), Identity: "account-identity", Model: "gpt-6-astra", Blocks: 10}
			generation, records, err := repo.Snapshot(ctx, scope)
			if err != nil || len(records) != 0 {
				t.Fatalf("初始记录异常: %v", err)
			}
			feedback := harvest.CodexHarvestNodeFeedback{Scope: scope, Node: mihomo.HarvestNode{ID: "node", Name: "VN node", Provider: "test"}, Generation: generation, Result: "success", LatencyMS: 40, CooldownSeconds: 60}
			if stored, err := repo.Record(ctx, feedback); err != nil || !stored {
				t.Fatalf("成功记录未保存: %v", err)
			}
			feedback.Result = "network_error"
			if stored, err := repo.Record(ctx, feedback); err != nil || !stored {
				t.Fatalf("失败记录未保存: %v", err)
			}
			_, records, err = repo.Snapshot(ctx, scope)
			if err != nil || len(records) != 1 || records[0].Successes != 1 || records[0].NetworkErrors != 1 || records[0].CooldownUntil == nil {
				t.Fatalf("节点统计异常: %+v %v", records, err)
			}
			other := scope
			other.Identity = "new-identity"
			_, otherRecords, err := repo.Snapshot(ctx, other)
			if err != nil || len(otherRecords) != 0 {
				t.Fatal("换身份后复用了旧学习记录")
			}
			if err = repo.Reset(ctx, records[0].ID); err != nil {
				t.Fatal(err)
			}
			if stored, err := repo.Record(ctx, feedback); err != nil || stored {
				t.Fatalf("重置前的旧请求重新写入了记录: %v", err)
			}
			_, records, err = repo.Snapshot(ctx, scope)
			if err != nil || len(records) != 0 {
				t.Fatal("重置未生效")
			}
			event := harvest.Event{AccountID: scope.AccountID, JobID: "job-test", Model: "gpt-6-astra", Result: "success", Length: 292, ExpectedLength: 292}
			if err = repo.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
			page, err := repo.Events(ctx, scope.AccountID, "job-test", 0, 25)
			if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Length != 292 || page.Items[0].ID == 0 {
				t.Fatalf("事件未持久化: %+v %v", page, err)
			}
			page, err = repo.Events(ctx, scope.AccountID, "another-job", 0, 25)
			if err != nil || page.Total != 0 {
				t.Fatal("事件筛选未生效")
			}
		})
	}
}
