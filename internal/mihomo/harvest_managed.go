package mihomo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

func (s *DirectedSidecar) IsManaged() bool { return s.managed != nil }

func (s *DirectedSidecar) UsesNodesOnce() bool {
	if s.managed == nil {
		return false
	}
	s.managed.mu.Lock()
	defer s.managed.mu.Unlock()
	return s.managed.saved.UseOnce
}

func (s *DirectedSidecar) acquireManaged(ctx context.Context, node HarvestNode) (func(), error) {
	m := s.managed
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	nodes, err := s.Directory(ctx)
	found := false
	for _, current := range nodes {
		if current.ID == node.ID && current.RuntimeID == node.RuntimeID {
			found = true
			break
		}
	}
	if err != nil || !found {
		m.release()
		return nil, errors.New("定向采票节点已失效")
	}
	if err = m.reserveCollectionNode(node); err != nil {
		m.release()
		return nil, err
	}
	m.mu.Lock()
	secret := m.saved.Secret
	useOnce := m.saved.UseOnce
	m.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			defer m.release()
			if !useOnce {
				return
			}
			m.mu.Lock()
			snapshot := m.saved
			m.mu.Unlock()
			finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			config, err := m.config(snapshot)
			if err == nil {
				err = m.reload(finish, config, secret)
			}
			if err != nil {
				m.stop()
			}
		})
	}
	payload, _ := json.Marshal(map[string]string{"name": node.RuntimeID})
	if err = m.control(ctx, http.MethodPut, "/proxies/"+collectionGroup(0), secret, payload); err == nil {
		err = s.confirmManaged(ctx, node)
	}
	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (s *DirectedSidecar) confirmManaged(ctx context.Context, node HarvestNode) error {
	m := s.managed
	m.mu.Lock()
	nodes := managedNodes(m.saved, true)
	ready := !m.closed && m.state.Running
	secret, controller := m.saved.Secret, m.controllerURL
	m.mu.Unlock()
	found := false
	for _, current := range nodes {
		if current.ID == node.ID && current.RuntimeID == node.RuntimeID {
			found = true
			break
		}
	}
	if !ready || !found {
		return errors.New("票据节点身份已变化")
	}
	if controller == "" {
		controller = "http://127.0.0.1:9098"
	}
	client := &DirectedSidecar{Controller: controller, Secret: secret}
	var group directedProxy
	if err := client.control(ctx, http.MethodGet, "/proxies/"+collectionGroup(0), nil, &group); err != nil {
		return err
	}
	if group.Type != "Selector" || group.Now != node.RuntimeID {
		return errors.New("采票节点选择已变化")
	}
	return nil
}
