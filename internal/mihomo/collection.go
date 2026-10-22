package mihomo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

const defaultCollectLanes = 32
const collectPort = 17893

func collectionGroup(lane int) string { return fmt.Sprintf("CODEX-COLLECT-%d", lane) }
func collectionProxy(lane int) string { return fmt.Sprintf("http://127.0.0.1:%d", collectPort+lane) }

func managedManager() *Manager {
	var selected *Manager
	managers.Range(func(key, _ any) bool {
		m := key.(*Manager)
		m.mu.Lock()
		ready := !m.closed && m.state.Running
		m.mu.Unlock()
		if ready {
			selected = m
			return false
		}
		return true
	})
	return selected
}

func managedPoolID(m *Manager) string { return harvestDigest([]string{m.dir, "managed-collection"}) }

func CollectionAvailable() bool { return managedManager() != nil }

func managedNodes(s saved, allowUsed bool) []HarvestNode {
	var nodes []HarvestNode
	for _, entry := range s.Nodes {
		name, _ := entry["name"].(string)
		if name == "" || !countryAllowed(s, name) || (s.Disabled[name] != "" && !(allowUsed && s.Disabled[name] == "used")) {
			continue
		}
		display := s.NodeNames[name]
		if display == "" {
			display = name
		}
		nodes = append(nodes, HarvestNode{ID: harvestDigest(entry), Name: sanitizeNodeDisplayName(display), Provider: "managed", RuntimeID: name})
	}
	return nodes
}

// Collection 独占管理租约，每个通道使用自己的 Selector，互不改变出口。
type Collection struct {
	m        *Manager
	mu       sync.Mutex
	nodes    []HarvestNode
	next     int
	ports    []int
	extended bool
	closeErr error
	closed   bool
	once     sync.Once
}

func BeginCollection(ctx context.Context, proxy string, lanes int) (*Collection, error) {
	if proxy != Endpoint || lanes < 1 {
		return nil, errors.New("并行采票需要托管 Mihomo，通道数必须大于零")
	}
	m := managedManager()
	if m == nil {
		return nil, errors.New("托管 Mihomo 未运行")
	}
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	nodes := managedNodes(m.saved, false)
	ready := !m.closed && m.state.Running
	last := m.saved.CollectionLastNode
	snapshot := m.saved
	m.mu.Unlock()
	if !ready || len(nodes) == 0 {
		m.release()
		return nil, errors.New("没有可用的采票节点")
	}
	for index, node := range nodes {
		if node.ID == last {
			nodes = append(append([]HarvestNode{}, nodes[index+1:]...), nodes[:index+1]...)
			break
		}
	}
	c := &Collection{m: m, nodes: nodes, ports: make([]int, min(lanes, len(nodes)))}
	for lane := range c.ports {
		c.ports[lane] = collectPort + lane
	}
	if len(c.ports) > defaultCollectLanes {
		if err := c.extend(ctx, snapshot); err != nil {
			return nil, errors.Join(err, c.Close())
		}
	}
	return c, nil
}

func (c *Collection) LaneCount() int { return len(c.ports) }
func (c *Collection) PoolID() string { return managedPoolID(c.m) }

func (c *Collection) Next(ctx context.Context, lane int) (HarvestNode, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return HarvestNode{}, "", err
	}
	if c.closed || lane < 0 || lane >= len(c.ports) {
		return HarvestNode{}, "", errors.New("采票通道已关闭或无效")
	}
	if c.next >= len(c.nodes) {
		return HarvestNode{}, "", nil
	}
	node := c.nodes[c.next]
	c.next++
	if err := c.m.reserveCollectionNode(node); err != nil {
		return HarvestNode{}, "", err
	}
	c.m.mu.Lock()
	secret := c.m.saved.Secret
	c.m.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{"name": node.RuntimeID})
	if err := c.m.control(ctx, http.MethodPut, "/proxies/"+collectionGroup(lane), secret, payload); err != nil {
		return HarvestNode{}, "", err
	}
	return node, fmt.Sprintf("http://127.0.0.1:%d", c.ports[lane]), nil
}

func (m *Manager) reserveCollectionNode(node HarvestNode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.saved
	next.Disabled = copyNodeStates(next.Disabled)
	if next.UseOnce {
		next.Disabled[node.RuntimeID] = "used"
	}
	next.CollectionLastNode = node.ID
	data, err := json.Marshal(next)
	if err == nil {
		err = atomicWrite(filepath.Join(m.dir, "settings.json"), data, 0600)
	}
	if err != nil {
		return errors.New("采票节点预留失败")
	}
	m.saved = next
	return nil
}

func copyNodeStates(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (c *Collection) extend(ctx context.Context, snapshot saved) error {
	// Reserve all extra OS-assigned ports at once, never extending the fixed
	// range into the gateway's 17925–17932 listeners.
	var reservations []net.Listener
	defer func() {
		for _, listener := range reservations {
			_ = listener.Close()
		}
	}()
	for lane := defaultCollectLanes; lane < len(c.ports); lane++ {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			return errors.New("无法分配并行采票端口")
		}
		reservations = append(reservations, listener)
		port := listener.Addr().(*net.TCPAddr).Port
		if port == 3101 || port == 3102 || port == 9098 || (port >= collectPort && port < collectPort+defaultCollectLanes) || (port >= gatewayPort && port < gatewayPort+GatewayLanes) {
			return errors.New("分配的采票端口与保留端口冲突")
		}
		c.ports[lane] = port
	}
	config, err := c.m.collectionConfig(snapshot, c.ports)
	if err != nil {
		return err
	}
	for _, listener := range reservations {
		_ = listener.Close()
	}
	// A partial reload must also be restored before releasing the lease.
	c.extended = true
	if err := c.m.reload(ctx, config, snapshot.Secret); err != nil {
		return errors.New("无法启用动态采票通道")
	}
	return nil
}

func (c *Collection) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.closed = true
		defer c.m.release()
		c.m.mu.Lock()
		snapshot := c.m.saved
		c.m.mu.Unlock()
		if !snapshot.UseOnce && !c.extended {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		config, err := c.m.config(snapshot)
		if err == nil {
			err = c.m.reload(ctx, config, snapshot.Secret)
		}
		if err != nil {
			c.m.stop()
			c.m.mu.Lock()
			c.m.state.Running = false
			c.m.mu.Unlock()
			c.closeErr = errors.New("采票通道恢复失败，托管代理已停止")
		}
	})
	return c.closeErr
}

// PinNode 可回选已采过的节点，但管理员停用、失败或地区排除的节点仍不可用。
func PinNode(ctx context.Context, id, poolID string) (string, func(), error) {
	m := managedManager()
	if m == nil || managedPoolID(m) != poolID {
		return "", nil, errors.New("票据绑定的托管节点池已失效")
	}
	if err := m.acquire(ctx); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	nodes := managedNodes(m.saved, true)
	secret := m.saved.Secret
	ready := !m.closed && m.state.Running
	m.mu.Unlock()
	for _, node := range nodes {
		if ready && node.ID == id {
			payload, _ := json.Marshal(map[string]string{"name": node.RuntimeID})
			if err := m.control(ctx, http.MethodPut, "/proxies/"+collectionGroup(0), secret, payload); err != nil {
				m.release()
				return "", nil, err
			}
			var once sync.Once
			return collectionProxy(0), func() { once.Do(m.release) }, nil
		}
	}
	m.release()
	return "", nil, errors.New("票据绑定节点已停用或发生变化")
}
