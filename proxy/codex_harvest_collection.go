package proxy

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
)

type harvestCollectionExit struct {
	node     mihomo.HarvestNode
	proxyURL string
	proxyID  int64
}

// Ordinary proxies already have independent exits. They need neither sidecar
// listeners nor a global Mihomo lease. Useful lanes are bounded by membership.
type harvestCollection struct {
	manager  *CodexHarvestManager
	managed  *mihomo.Collection
	proxyIDs []int64
	lanes    int
	cursor   atomic.Uint64
}

func (m *CodexHarvestManager) beginHarvestCollection(ctx context.Context, controls harvest.CodexHarvestControls, lanes int) (*harvestCollection, error) {
	if lanes <= 0 {
		return nil, fmt.Errorf("采票请求预算不足")
	}
	c := &harvestCollection{manager: m, proxyIDs: slices.Clone(controls.ProxyIDs)}
	if len(c.proxyIDs) > 0 {
		exits, err := m.harvestProxyExits(ctx, c.proxyIDs, false)
		if err != nil {
			return nil, err
		}
		if len(exits) == 0 {
			return nil, fmt.Errorf("所选采票代理均不可用")
		}
		c.lanes = min(lanes, len(exits))
		return c, nil
	}
	managed, err := mihomo.BeginCollection(ctx, auth.ConfiguredCodexTicketSettings().HarvestProxyURL, lanes)
	if err != nil {
		return nil, err
	}
	c.managed, c.lanes = managed, managed.LaneCount()
	return c, nil
}

func (c *harvestCollection) Next(ctx context.Context, lane int) (harvestCollectionExit, error) {
	if c.managed != nil {
		node, proxyURL, err := c.managed.Next(ctx, lane)
		return harvestCollectionExit{node: node, proxyURL: proxyURL}, err
	}
	exits, err := c.manager.harvestProxyExits(ctx, c.proxyIDs, false)
	if err != nil {
		return harvestCollectionExit{}, err
	}
	if len(exits) == 0 {
		return harvestCollectionExit{}, fmt.Errorf("所选采票代理均不可用")
	}
	exit := exits[(c.cursor.Add(1)-1)%uint64(len(exits))]
	return harvestCollectionExit{node: mihomo.HarvestNode{ID: exit.key(), Name: exit.name(), Provider: "proxy"}, proxyURL: exit.url, proxyID: exit.id}, nil
}

func (c *harvestCollection) bind(ticket *auth.CodexTicket, exit harvestCollectionExit) {
	if c.managed != nil {
		// Temporary expanded listeners disappear at Close. Stored tickets pin
		// their node again, using the stable managed endpoint instead.
		ticket.HarvestProxyURL = mihomo.Endpoint
		ticket.HarvestNodeID, ticket.HarvestNodeName = exit.node.ID, exit.node.Name
		ticket.HarvestPoolID, ticket.ManagedNode = c.managed.PoolID(), true
	}
}

func (c *harvestCollection) Close() error {
	if c.managed != nil {
		return c.managed.Close()
	}
	return nil
}
