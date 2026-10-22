package mihomo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

const GatewayLanes = 8
const gatewayPort = 17925

var gatewayCursor atomic.Uint64

func gatewayGroup(lane int) string { return fmt.Sprintf("CODEX-GATEWAY-%d", lane) }
func appendGatewayListeners(groups, listeners []any, names []string) ([]any, []any) {
	for lane := 0; lane < GatewayLanes; lane++ {
		name := gatewayGroup(lane)
		groups = append(groups, map[string]any{"name": name, "type": "select", "proxies": names})
		listeners = append(listeners, map[string]any{"name": name, "type": "mixed", "listen": "127.0.0.1", "port": gatewayPort + lane, "proxy": name})
	}
	return groups, listeners
}

// GatewayNodes has the same country and administrator-disabled filters as
// collection. Gateway routing must not consume use-once collection nodes.
func GatewayNodes() ([]HarvestNode, string, error) {
	m := managedManager()
	if m == nil {
		return nil, "", errors.New("node_unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saved.UseOnce {
		return nil, "", errors.New("rotation_use_once")
	}
	return managedNodes(m.saved, false), managedPoolID(m), nil
}
func PinGatewayNode(ctx context.Context, id, poolID string) (string, func(), error) {
	m := managedManager()
	if m == nil || managedPoolID(m) != poolID {
		return "", func() {}, errors.New("node_unavailable")
	}
	if err := m.acquire(ctx); err != nil {
		return "", func() {}, err
	}
	m.mu.Lock()
	nodes := managedNodes(m.saved, false)
	ready := !m.closed && m.state.Running && !m.saved.UseOnce
	secret := m.saved.Secret
	m.mu.Unlock()
	lane := int(gatewayCursor.Add(1)-1) % GatewayLanes
	var once sync.Once
	release := func() { once.Do(m.release) }
	for _, node := range nodes {
		if ready && node.ID == id {
			raw, _ := json.Marshal(map[string]string{"name": node.RuntimeID})
			if err := m.control(ctx, http.MethodPut, "/proxies/"+gatewayGroup(lane), secret, raw); err != nil {
				release()
				return "", func() {}, err
			}
			return fmt.Sprintf("http://127.0.0.1:%d", gatewayPort+lane), release, nil
		}
	}
	release()
	return "", func() {}, errors.New("node_unavailable")
}
