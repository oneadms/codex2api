package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
)

func TestGatewayWSHistoryDistinguishesOwnMaterialAndMissingSource(t *testing.T) {
	m, source, target := newGatewayWSUnitFixture(t)
	for _, status := range []string{"own_ticket", "manual_ticket", "unavailable", "selected"} {
		t.Run(status, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			ctx = BeginCodexGatewayWSObservation(ctx, target, gatewayModel)
			if status != "unavailable" {
				ctx = context.WithValue(ctx, gatewayRequestKey{}, gatewayRequest{
					accountID: target.ID(), model: gatewayModel, websocket: true,
					route: gatewayRoute{sourceID: source.ID(), cookie: "private-source-cookie"},
				})
			}
			switch status {
			case "own_ticket":
				ctx = context.WithValue(ctx, codexTicketInjectionKey{}, &auth.CodexTicket{State: "private-own-state"})
				ctx = withCodexTurnStateInjection(ctx, "private-own-state")
			case "manual_ticket":
				ctx = withCodexTurnStateInjection(ctx, "private-manual-state")
			}
			if status != "selected" {
				ObserveCodexGatewayWSHandshake(ctx, http.Header{"Cookie": {"__oailb=private-target-cookie"}}, false)
				ObserveCodexGatewayWSFrame(ctx, []byte(`{"type":"response.completed","response":{"model":"gpt-6-astra","status":"completed","error":null}}`))
			}
			// 取消及并发关闭不能漏记结果，也不能让同一请求产生两条历史。
			cancel()
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() { FinishCodexGatewayWSObservation(ctx, status == "selected") })
			}
			wg.Wait()
			page, err := m.Repository.GatewayHistory(t.Context(), 0, 25)
			if err != nil || len(page.Items) == 0 {
				t.Fatalf("取消后未保留历史: %v", err)
			}
			record := page.Items[0]
			if record.BorrowStatus != status || record.BorrowedCookieSent || record.Success != (status != "selected") {
				t.Fatalf("自有材料或无来源请求不可记成借用成功: %+v", record)
			}
			if (status == "selected") != (record.SourceAccountID == source.ID()) {
				t.Fatalf("自有材料不得归到来源账号: %+v", record)
			}
			raw, _ := json.Marshal(record)
			if strings.Contains(string(raw), "private-") {
				t.Fatal("历史泄漏了票体或 Cookie 内容")
			}
		})
	}
	page, err := m.Repository.GatewayHistory(t.Context(), 0, 25)
	if err != nil || page.Total != 4 {
		t.Fatalf("请求历史必须只写入一次: count=%d err=%v", page.Total, err)
	}
}
