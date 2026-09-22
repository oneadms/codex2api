package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/codex2api/internal/mihomo"
)

// 定向票据持有 Selector 直到响应关闭。拨号、重连与换节点不能并发改变出口。
func acquireCodexTicketEgress(ctx context.Context) (context.Context, func(), error) {
	ticket := codexTicketFromContext(ctx)
	if ticket == nil || ticket.HarvestNodeID == "" {
		return ctx, func() {}, nil
	}
	m := codexHarvester.Load()
	if m == nil {
		return ctx, nil, fmt.Errorf("票据的节点管理器不可用")
	}
	sidecar, err := mihomo.LoadDirectedSidecar(m.DataDir, ticket.HarvestProxyURL)
	if err != nil || sidecar.PoolID != ticket.HarvestPoolID {
		return ctx, nil, fmt.Errorf("票据的节点池已变化，请重新采票")
	}
	node, ok := sidecar.Lookup(ctx, ticket.HarvestNodeID, "")
	if !ok {
		return ctx, nil, fmt.Errorf("票据绑定节点已失效，请重新采票")
	}
	release, err := sidecar.Acquire(ctx, node)
	if err != nil {
		return ctx, nil, fmt.Errorf("票据绑定节点暂不可用")
	}
	return WithFreshCodexConnection(ctx), release, nil
}

type codexTicketLeaseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *codexTicketLeaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func retainCodexTicketEgress(ctx context.Context, response *http.Response, release func()) {
	if ticket := codexTicketFromContext(ctx); ticket == nil || ticket.HarvestNodeID == "" {
		release()
		return
	}
	if response == nil || response.Body == nil {
		release()
		return
	}
	var once sync.Once
	finish := func() { once.Do(release) }
	stop := context.AfterFunc(ctx, finish)
	response.Body = &codexTicketLeaseBody{ReadCloser: response.Body, release: func() { stop(); finish() }}
}
