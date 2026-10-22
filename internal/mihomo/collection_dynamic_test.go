package mihomo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type collectionConfigFixture struct {
	Listeners []struct {
		Name   string `json:"name"`
		Listen string `json:"listen"`
		Port   int    `json:"port"`
	} `json:"listeners"`
}

func collectionManagerFixture(t *testing.T, controller string, count int) *Manager {
	t.Helper()
	m := New(t.TempDir())
	t.Cleanup(m.Close)
	m.controllerURL = controller
	m.mu.Lock()
	m.state.Running = true
	m.saved = saved{Secret: "fixture-secret"}
	for i := 0; i < count; i++ {
		m.saved.Nodes = append(m.saved.Nodes, map[string]any{"name": fmt.Sprintf("node-%d", i), "type": "http", "server": "offline.invalid", "port": 80 + i})
	}
	m.mu.Unlock()
	return m
}

func TestCollectionDynamicLanesAvoidGatewayPortsAndRestore(t *testing.T) {
	var mu sync.Mutex
	var configs []collectionConfigFixture
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/configs" {
			var envelope map[string]string
			if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			var cfg collectionConfigFixture
			if err := json.Unmarshal([]byte(envelope["payload"]), &cfg); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			mu.Lock()
			configs = append(configs, cfg)
			mu.Unlock()
		}
		w.WriteHeader(204)
	}))
	defer controller.Close()
	collectionManagerFixture(t, controller.URL, 80)
	collection, err := BeginCollection(t.Context(), Endpoint, 64)
	require.NoError(t, err)
	require.Equal(t, 64, collection.LaneCount())
	defer collection.Close()
	mu.Lock()
	require.Len(t, configs, 1)
	cfg := configs[0]
	mu.Unlock()
	require.Len(t, cfg.Listeners, 64+GatewayLanes)
	seen := map[int]bool{}
	for i, listener := range cfg.Listeners {
		require.Equal(t, "127.0.0.1", listener.Listen)
		require.False(t, seen[listener.Port])
		seen[listener.Port] = true
		if i < 64 {
			require.False(t, listener.Port >= gatewayPort && listener.Port < gatewayPort+GatewayLanes)
		}
	}
	node, proxy, err := collection.Next(t.Context(), 63)
	require.NoError(t, err)
	require.NotEmpty(t, node.ID)
	parsed, err := url.Parse(proxy)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	require.Equal(t, cfg.Listeners[63].Port, port)
	require.NoError(t, collection.Close())
	require.NoError(t, collection.Close())
	mu.Lock()
	require.Len(t, configs, 2)
	require.Len(t, configs[1].Listeners, defaultCollectLanes+GatewayLanes)
	mu.Unlock()
	// The persisted bundle repins by node, not the retired temporary listener.
	_, release, err := PinNode(t.Context(), node.ID, collection.PoolID())
	require.NoError(t, err)
	release()
}

func TestCollectionFailedExtensionRestoresAndReleasesGate(t *testing.T) {
	var reloads atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/configs" && reloads.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer controller.Close()
	collectionManagerFixture(t, controller.URL, 40)
	_, err := BeginCollection(t.Context(), Endpoint, 40)
	require.Error(t, err)
	require.Equal(t, int32(2), reloads.Load())
	next, err := BeginCollection(t.Context(), Endpoint, 2)
	require.NoError(t, err)
	require.NoError(t, next.Close())
}

func TestCollectionRequestedLanesBoundedByNodes(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer controller.Close()
	collectionManagerFixture(t, controller.URL, 2)
	collection, err := BeginCollection(t.Context(), Endpoint, 1<<30)
	require.NoError(t, err)
	require.Equal(t, 2, collection.LaneCount())
	require.NoError(t, collection.Close())
}
