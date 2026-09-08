package database

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestNormalizeVisibleChannels(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil means everything visible", nil, []string{"codex", "claude", "antigravity", "grok", "traecn"}},
		{"empty list keeps the fallback", []string{}, []string{"codex"}},
		{"fallback is added when missing", []string{"grok"}, []string{"codex", "grok"}},
		{"traecn is selectable and ordered after the upstream channels", []string{"traecn"}, []string{"codex", "traecn"}},
		{"unknown, blank and duplicate entries are dropped and order is canonical", []string{" Grok ", "", "claude", "grok", "openai"}, []string{"codex", "claude", "grok"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeVisibleChannels(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NormalizeVisibleChannels(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestVisibleChannelsConfigRoundTrip(t *testing.T) {
	// 显式空列表必须序列化出 channels 字段，否则读回来会被当成「从未配置」而全部显示。
	raw, err := json.Marshal(VisibleChannelsConfig{Channels: NormalizeVisibleChannels([]string{})})
	if err != nil {
		t.Fatal(err)
	}
	var cfg VisibleChannelsConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Effective(); !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("round trip of explicit fallback-only list = %v, raw=%s", got, raw)
	}
	var missing VisibleChannelsConfig
	if err := json.Unmarshal([]byte(`{}`), &missing); err != nil {
		t.Fatal(err)
	}
	if got := missing.Effective(); len(got) != len(AllUpstreamChannels) {
		t.Fatalf("missing field should mean all visible, got %v", got)
	}
}

// 上游每新增一个渠道，都要同时进 AllUpstreamChannels 和前端
// frontend/src/lib/visibleChannels.ts 的 ALL_VISIBLE_CHANNEL_OPTIONS，
// 否则管理台会把该渠道整条过滤掉（TRAE CN 就这样从仪表盘/账号页消失过）。
func TestAllUpstreamChannelsCoversEveryChannel(t *testing.T) {
	for _, channel := range []string{
		UpstreamChannelCodex,
		UpstreamChannelGrok,
		UpstreamChannelAntigravity,
		UpstreamChannelClaude,
		UpstreamChannelTraeCN,
	} {
		if !slices.Contains(AllUpstreamChannels, channel) {
			t.Fatalf("AllUpstreamChannels 缺少渠道 %q: %v", channel, AllUpstreamChannels)
		}
	}
}
