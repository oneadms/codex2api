package mihomo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCollectionIsolatesLanesAndPreservesUsedNodes(t *testing.T) {
	var mu sync.Mutex
	selected := map[string]string{}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/configs" {
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			mu.Lock()
			selected[r.URL.Path] = body["name"]
			mu.Unlock()
		}
		w.WriteHeader(204)
	}))
	defer controller.Close()
	m := New(t.TempDir())
	defer m.Close()
	m.controllerURL = controller.URL
	m.mu.Lock()
	m.state.Running = true
	m.saved = saved{UseOnce: true, Secret: "test", Disabled: map[string]string{"blocked": "disabled"}, Nodes: []map[string]any{{"name": "a", "type": "http", "server": "a.example", "port": 80}, {"name": "b", "type": "http", "server": "b.example", "port": 80}, {"name": "blocked", "type": "http", "server": "c.example", "port": 80}}}
	m.mu.Unlock()
	collection, err := BeginCollection(t.Context(), Endpoint, 10)
	require.NoError(t, err)
	require.Equal(t, 2, collection.LaneCount())
	first, proxy1, err := collection.Next(t.Context(), 0)
	require.NoError(t, err)
	second, proxy2, err := collection.Next(t.Context(), 1)
	require.NoError(t, err)
	require.NotEqual(t, proxy1, proxy2)
	require.NotEqual(t, first.ID, second.ID)
	none, _, err := collection.Next(t.Context(), 0)
	require.NoError(t, err)
	require.Empty(t, none.ID)
	raw, err := os.ReadFile(filepath.Join(m.dir, "settings.json"))
	require.NoError(t, err)
	var persisted saved
	require.NoError(t, json.Unmarshal(raw, &persisted))
	require.Equal(t, "used", persisted.Disabled[first.RuntimeID])
	require.Equal(t, "used", persisted.Disabled[second.RuntimeID])
	wait, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err = BeginCollection(wait, Endpoint, 2)
	require.Error(t, err)
	collection.Close()
	collection.Close()
	_, release, err := PinNode(t.Context(), first.ID, collection.PoolID())
	require.NoError(t, err)
	release()
	m.mu.Lock()
	m.saved.Disabled[first.RuntimeID] = "disabled"
	m.mu.Unlock()
	_, _, err = PinNode(t.Context(), first.ID, collection.PoolID())
	require.Error(t, err)
	mu.Lock()
	require.Len(t, selected, 2)
	mu.Unlock()
}

func TestCollectionListenersStayOnLoopback(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	raw, err := m.config(saved{Nodes: []map[string]any{{"name": "a", "type": "http", "server": "a.example", "port": 80}}})
	require.NoError(t, err)
	var config struct {
		Listeners []struct {
			Listen string `json:"listen"`
			Port   int    `json:"port"`
			Proxy  string `json:"proxy"`
		} `json:"listeners"`
	}
	require.NoError(t, json.Unmarshal(raw, &config))
	require.Len(t, config.Listeners, MaxCollectLanes+GatewayLanes)
	for lane, listener := range config.Listeners {
		require.Equal(t, "127.0.0.1", listener.Listen)
		if lane < MaxCollectLanes {
			require.Equal(t, collectPort+lane, listener.Port)
			require.Equal(t, collectionGroup(lane), listener.Proxy)
			continue
		}
		gatewayLane := lane - MaxCollectLanes
		require.Equal(t, gatewayPort+gatewayLane, listener.Port)
		require.Equal(t, gatewayGroup(gatewayLane), listener.Proxy)
	}
}
