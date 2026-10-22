package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
)

func newGatewayRegressionFixture(t *testing.T) (*CodexHarvestManager, *auth.Account, *auth.Account, harvest.GatewaySettings) {
	t.Helper()
	m, target := newHarvestManagerTest(t)
	settings := auth.ConfiguredCodexTicketSettings()
	settings.TargetLength = 292
	auth.SetConfiguredCodexTicketSettings(settings)
	id, err := m.DB.InsertAccountWithCredentials(t.Context(), "gateway-donor", map[string]any{"upstream_type": "codex", "access_token": "offline-source-token", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Store.LoadAccountByID(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	v := harvest.DefaultGatewaySettings()
	v.CookiePool.Enabled = true
	v.CookiePool.SourceAccountIDs = []int64{id}
	v.CookiePool.TargetAccountIDs = []int64{target.ID()}
	v.Revision, v.RouteRevision = "settings-initial", "route-initial"
	seedGatewayRegressionSettings(t, m, v)
	return m, m.Store.FindByID(id), target, v
}

func seedGatewayRegressionSettings(t *testing.T, m *CodexHarvestManager, v harvest.GatewaySettings) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := m.Repository.Set(t.Context(), harvest.GatewaySettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	g := m.gatewayManager()
	g.mu.Lock()
	g.loadedUntil = time.Time{}
	g.mu.Unlock()
	if _, err := m.GatewaySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func seedGatewayRegressionRoute(g *codexGatewayManager, source *auth.Account) gatewayRoute {
	r := gatewayRoute{sourceID: source.ID(), identity: gatewayIdentity(source), cookie: "offline-route-cookie", expires: time.Now().Add(2 * time.Minute)}
	g.mu.Lock()
	g.routes[source.ID()], g.activeSource = r, source.ID()
	g.mu.Unlock()
	return r
}

func TestCodexGatewaySchedulingPreservesRouteGeneration(t *testing.T) {
	m, source, _, v := newGatewayRegressionFixture(t)
	g := m.gatewayManager()
	r := seedGatewayRegressionRoute(g, source)
	anchor := &gatewayAnchor{expires: time.Now().Add(time.Minute)}
	g.mu.Lock()
	g.anchors["existing"] = anchor
	g.mu.Unlock()
	v.TicketPoolScheduling = true
	saved, err := m.SaveGatewaySettings(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision == v.Revision || gatewayRouteRevision(saved) != gatewayRouteRevision(v) {
		t.Fatalf("settings and route revisions must be independent: before=%+v after=%+v", v, saved)
	}
	g.mu.Lock()
	preserved := g.anchors["existing"] == anchor && gatewayFingerprint(g.routes[source.ID()]) == gatewayFingerprint(r)
	g.mu.Unlock()
	if !preserved {
		t.Fatal("scheduling save discarded route/session")
	}
	if _, err = m.SaveGatewaySettings(t.Context(), v); !errors.Is(err, ErrGatewayRevisionConflict) {
		t.Fatalf("stale scheduling draft accepted: %v", err)
	}
	saved.CookiePool.TTLSeconds--
	next, err := m.SaveGatewaySettings(t.Context(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if gatewayRouteRevision(next) == gatewayRouteRevision(saved) {
		t.Fatal("route edit failed to advance route revision")
	}
}

func TestCodexGatewaySettingsCASAcrossManagers(t *testing.T) {
	m, _, _, v := newGatewayRegressionFixture(t)
	other := &CodexHarvestManager{DB: m.DB, Store: m.Store, Repository: m.Repository, Controls: m.Controls}
	old, err := other.GatewaySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	v.TicketPoolScheduling = true
	if _, err = m.SaveGatewaySettings(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	old.SchedulingMode = "model"
	if _, err = other.SaveGatewaySettings(t.Context(), old); !errors.Is(err, ErrGatewayRevisionConflict) {
		t.Fatalf("other process overwrote newer configuration: %v", err)
	}
	fresh, err := other.GatewaySettings(t.Context())
	if err != nil || !fresh.TicketPoolScheduling {
		t.Fatalf("conflict did not reload latest configuration: %+v %v", fresh, err)
	}
}

func TestCodexGatewaySnapshotRechecksRouteAndIdentity(t *testing.T) {
	for _, change := range []string{"cookie", "source_identity", "target_identity", "expired", "deleted"} {
		t.Run(change, func(t *testing.T) {
			m, source, target, _ := newGatewayRegressionFixture(t)
			g := m.gatewayManager()
			r := seedGatewayRegressionRoute(g, source)
			g.mu.Lock()
			g.checks[target.ID()] = gatewayCheck{sourceID: source.ID(), identity: gatewayIdentity(target), fingerprint: gatewayFingerprint(r), status: harvest.GatewayRouteStatus{AccountID: target.ID(), State: "ready", Reason: "target_probe_passed", ExpiresAt: &r.expires}}
			g.mu.Unlock()
			if m.GatewaySnapshot(t.Context()).ReadyRoutes != 1 {
				t.Fatal("fixture did not start ready")
			}
			switch change {
			case "cookie":
				r.cookie = "replacement"
			case "source_identity":
				source.Mu().Lock()
				source.Email = "changed@example.invalid"
				source.Mu().Unlock()
			case "target_identity":
				target.Mu().Lock()
				target.Email = "changed@example.invalid"
				target.Mu().Unlock()
			case "expired":
				r.expires = time.Now().Add(-time.Second)
			}
			g.mu.Lock()
			if change == "deleted" {
				delete(g.routes, source.ID())
			} else {
				g.routes[source.ID()] = r
			}
			g.mu.Unlock()
			snapshot := m.GatewaySnapshot(t.Context())
			if snapshot.ReadyRoutes != 0 || snapshot.Targets[0].State == "ready" {
				t.Fatalf("stale evidence remained ready: %+v", snapshot.Targets)
			}
		})
	}
}

func TestCodexGatewayProbeRechecksAfterContinuation(t *testing.T) {
	for _, change := range []string{"target_identity", "source_identity", "route"} {
		t.Run(change, func(t *testing.T) {
			m, source, target, v := newGatewayRegressionFixture(t)
			g := m.gatewayManager()
			route := seedGatewayRegressionRoute(g, source)
			var shots atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if shots.Add(1) == 1 {
					w.Header().Set(codexTurnStateHeader, testTicketState(time.Now(), auth.CodexTicketPersonalBlocks))
				} else {
					switch change {
					case "target_identity":
						target.Mu().Lock()
						target.Email = "changed@example.invalid"
						target.Mu().Unlock()
					case "source_identity":
						source.Mu().Lock()
						source.Email = "changed@example.invalid"
						source.Mu().Unlock()
					case "route":
						route.cookie = "new-route"
						g.mu.Lock()
						g.routes[source.ID()] = route
						g.mu.Unlock()
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-astra\",\"status\":\"completed\"}}\n\n")
			}))
			defer server.Close()
			previous := codexTicketProbeURLForTest
			codexTicketProbeURLForTest = server.URL
			t.Cleanup(func() { codexTicketProbeURLForTest = previous })
			if err := g.verify(t.Context(), v, target.ID(), false); err == nil {
				t.Fatal("stale identity/route probe was accepted")
			}
			if shots.Load() != 2 {
				t.Fatalf("expected two offline probes, got %d", shots.Load())
			}
			if m.GatewaySnapshot(t.Context()).ReadyRoutes != 0 {
				t.Fatal("stale probe published readiness")
			}
		})
	}
}

func TestCodexGatewayRotationKeepsOtherDonorsEligible(t *testing.T) {
	m, source, _, v := newGatewayRegressionFixture(t)
	secondID, err := m.DB.InsertAccountWithCredentials(t.Context(), "second-donor", map[string]any{"upstream_type": "codex", "access_token": "second-token", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Store.LoadAccountByID(t.Context(), secondID); err != nil {
		t.Fatal(err)
	}
	v.CookiePool.SourceAccountIDs = append(v.CookiePool.SourceAccountIDs, secondID)
	v.CookiePool.RotateNodes, v.CookiePool.IPAffinity = true, true
	v.CookiePool.MaxNodeAttempts = 1
	seedGatewayRegressionSettings(t, m, v)
	g := m.gatewayManager()
	var first, second atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+source.GetAccessToken() {
			first.Add(1)
			w.WriteHeader(500)
			return
		}
		second.Add(1)
		w.Header().Set("Set-Cookie", "__oailb=second-cookie; Secure; Path=/; Max-Age=200")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-astra\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	g.listNodes = func() ([]mihomo.HarvestNode, string, error) {
		return []mihomo.HarvestNode{{ID: "node-1", Name: "one"}}, "offline", nil
	}
	g.pinNode = func(context.Context, string, string) (string, func(), error) { return server.URL, func() {}, nil }
	previous := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = "http://gateway.invalid/backend-api/codex/responses"
	t.Cleanup(func() { codexTicketProbeURLForTest = previous })
	if err = g.prepare(t.Context(), v); err != nil {
		t.Fatalf("one failed donor prevented backup donor: %v", err)
	}
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("donor attempts: first=%d second=%d", first.Load(), second.Load())
	}
	if route, ok := g.pickRoute(v); !ok || route.sourceID != secondID {
		t.Fatal("backup route was not admitted")
	}
}

func TestCodexGatewayRotationScansPastCoolingNodes(t *testing.T) {
	m, _, _, v := newGatewayRegressionFixture(t)
	v.CookiePool.RotateNodes, v.CookiePool.IPAffinity = true, true
	v.CookiePool.MaxNodeAttempts = 1
	seedGatewayRegressionSettings(t, m, v)
	g := m.gatewayManager()
	g.listNodes = func() ([]mihomo.HarvestNode, string, error) {
		return []mihomo.HarvestNode{{ID: "cold"}, {ID: "available"}}, "offline", nil
	}
	g.nodeCooldown["cold"] = time.Now().Add(time.Hour)
	selected := ""
	g.pinNode = func(_ context.Context, id, _ string) (string, func(), error) {
		selected = id
		return "", func() {}, errors.New("offline stop after selection")
	}
	_ = g.prepare(t.Context(), v)
	if selected != "available" {
		t.Fatalf("node budget was consumed by a cooling node: selected=%q", selected)
	}
}
