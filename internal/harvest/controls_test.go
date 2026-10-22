package harvest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type controlsTestSettings struct{ values map[string]string }

func (s *controlsTestSettings) GetValue(_ context.Context, key string) (string, error) {
	v, ok := s.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}
func (s *controlsTestSettings) Set(_ context.Context, key, value string) error {
	s.values[key] = value
	return nil
}

func TestHarvestControlsProxyIDsSnapshotsAreDetached(t *testing.T) {
	repo := &controlsTestSettings{values: map[string]string{}}
	service := NewCodexHarvestService(nil, repo, nil)
	v := CodexHarvestControls{Version: 1, ProxyIDs: []int64{11, 22}, Speed: CodexHarvestSpeedPresets()["burst"]}
	if err := service.SaveControls(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	v.ProxyIDs[0] = 99
	got, _, err := service.Controls(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyIDs[0] != 11 {
		t.Fatal("saved settings retained caller's slice")
	}
	got.ProxyIDs[0] = 98
	again, _, _ := service.Controls(t.Context())
	if again.ProxyIDs[0] != 11 {
		t.Fatal("read snapshot mutated active pool")
	}
	raw, _ := json.Marshal(again)
	if !strings.Contains(string(raw), `"proxy_ids":[11,22]`) || strings.Contains(string(raw), "proxy_pool_id") {
		t.Fatal("old inert proxy selector still exposed")
	}
}

func TestHarvestManualLimitsAreBudgetBoundedNotFixed(t *testing.T) {
	r := ManualRequest{AccountID: 1, Models: []string{"gpt-6-astra"}, CollectLanes: 4096, MaxAttempts: 10000, RateLimitCooldownSeconds: 1, NodeSwitchRule: "every_request"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.CollectLanes = -1
	if err := r.Validate(); err == nil {
		t.Fatal("negative lanes accepted")
	}
	r.CollectLanes = 1
	r.MaxAttempts = 0
	if err := r.Validate(); err == nil {
		t.Fatal("zero budget accepted")
	}
}
