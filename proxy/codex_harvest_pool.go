package proxy

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
)

// A pool member is a concrete proxy row, not a sidecar selector. Only its ID is
// exposed in diagnostics; the URL (including authentication) stays private.
type harvestProxyExit struct {
	id  int64
	url string
}

func (e harvestProxyExit) key() string  { return fmt.Sprintf("proxy:%d", e.id) }
func (e harvestProxyExit) name() string { return fmt.Sprintf("代理 #%d", e.id) }

func harvestProxyEligible(row *database.ProxyRow) bool {
	return row != nil && row.Enabled && row.TestStatus != "error" && strings.TrimSpace(row.URL) != "" && auth.ValidateCodexTicketProxyURL(row.URL) == nil
}

func (m *CodexHarvestManager) harvestProxyExits(ctx context.Context, ids []int64, strict bool) ([]harvestProxyExit, error) {
	rows, err := m.DB.ListProxiesByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("读取采票代理列表失败")
	}
	byID := make(map[int64]*database.ProxyRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	exits := make([]harvestProxyExit, 0, len(ids))
	for _, id := range ids {
		row := byID[id]
		if !harvestProxyEligible(row) {
			if strict {
				return nil, fmt.Errorf("采票代理 #%d 不存在、未启用、检测失败或地址无效", id)
			}
			continue
		}
		exits = append(exits, harvestProxyExit{id: id, url: strings.TrimSpace(row.URL)})
	}
	return exits, nil
}

// SaveControls is the admin save entry point: validate selected IDs against the
// current proxy rows before persisting or publishing any configuration.
func (m *CodexHarvestManager) SaveControls(ctx context.Context, controls harvest.CodexHarvestControls) error {
	if err := harvest.ValidateCodexHarvestControls(controls); err != nil {
		return err
	}
	if _, err := m.harvestProxyExits(ctx, controls.ProxyIDs, true); err != nil {
		return err
	}
	if err := m.Controls.SaveControls(ctx, controls); err != nil {
		return err
	}
	auth.SetCodexTicketProxyPoolConfigured(len(controls.ProxyIDs) > 0)
	return nil
}

// ParallelAvailable also supports ordinary proxy pools, without a Mihomo process.
func (m *CodexHarvestManager) ParallelAvailable(ctx context.Context) bool {
	controls, _, err := m.Controls.Controls(ctx)
	if err != nil {
		return false
	}
	if len(controls.ProxyIDs) > 0 {
		exits, err := m.harvestProxyExits(ctx, controls.ProxyIDs, false)
		return err == nil && len(exits) > 0
	}
	return auth.ConfiguredCodexTicketSettings().HarvestProxyURL == mihomo.Endpoint && mihomo.CollectionAvailable()
}

func (m *CodexHarvestManager) harvestProxyCurrent(ctx context.Context, id int64, proxyURL string) bool {
	controls, _, err := m.Controls.Controls(ctx)
	if err != nil || !slices.Contains(controls.ProxyIDs, id) {
		return false
	}
	row, err := m.DB.GetProxy(ctx, id)
	return err == nil && harvestProxyEligible(row) && strings.TrimSpace(row.URL) == proxyURL
}

func (m *CodexHarvestManager) selectPoolProxy(ctx context.Context, account *auth.Account, model string, controls harvest.CodexHarvestControls, tried map[string]bool, keep **harvestSelection, manual bool) (*harvestSelection, string) {
	exits, err := m.harvestProxyExits(ctx, controls.ProxyIDs, false)
	if err != nil || len(exits) == 0 {
		return nil, "proxy_pool_unavailable"
	}
	choose := func(e harvestProxyExit, reason string) (*harvestSelection, string) {
		return &harvestSelection{proxyID: e.id, proxyURL: e.url}, reason
	}
	if keep != nil && *keep != nil && (*keep).proxyID > 0 {
		for _, exit := range exits {
			if exit.id == (*keep).proxyID && exit.url == (*keep).proxyURL {
				return choose(exit, "manual_sticky")
			}
		}
	}
	if !manual {
		if ticket := account.CodexTicketForModel(model, time.Now(), 0, controls.Transport); ticket != nil {
			for _, exit := range exits {
				if exit.url == ticket.HarvestProxyURL && !tried[exit.key()] {
					return choose(exit, "ticket_sticky")
				}
			}
		}
	}
	start := m.proxyCursor.Add(1) - 1
	for i := range exits {
		exit := exits[(start+uint64(i))%uint64(len(exits))]
		if !tried[exit.key()] {
			return choose(exit, "proxy_pool")
		}
	}
	if manual {
		// A manual run can make another pass after exhausting the selected pool.
		for _, exit := range exits {
			delete(tried, exit.key())
		}
		return choose(exits[start%uint64(len(exits))], "proxy_pool")
	}
	return nil, "proxy_pool_exhausted"
}
