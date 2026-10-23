package proxy

import (
	"errors"
	"slices"
	"testing"

	"github.com/codex2api/internal/harvest"
)

func TestCodexGatewaySaveRemovesDeletedAccounts(t *testing.T) {
	for _, deleted := range []string{"source", "target", "disabled"} {
		t.Run(deleted, func(t *testing.T) {
			m, source, target, v := newGatewayRegressionFixture(t)
			g := m.gatewayManager()
			g.mu.Lock()
			g.stopped = true // Settings tests must never start an upstream probe.
			g.mu.Unlock()
			v.WSSession.Enabled = true
			v.WSSession.AccountIDs = []int64{target.ID()}
			if deleted == "disabled" {
				v.CookiePool.Enabled, v.WSSession.Enabled = false, false
			}
			seedGatewayRegressionSettings(t, m, v)
			if deleted == "source" {
				m.Store.RemoveAccount(source.ID())
			} else {
				m.Store.RemoveAccount(target.ID())
			}
			v.TicketPoolScheduling = true
			saved, err := m.SaveGatewaySettings(t.Context(), v)
			if err != nil {
				t.Fatalf("deleted account blocked settings recovery: %v", err)
			}
			if saved.CookiePool.Enabled || saved.WSSession.Enabled {
				t.Fatalf("incomplete borrowing configuration must be disabled: %+v", saved)
			}
			if !saved.TicketPoolScheduling || saved.CookiePool.TTLSeconds != 230 {
				t.Fatal("cleanup lost unrelated settings")
			}
			if deleted == "source" {
				if len(saved.CookiePool.SourceAccountIDs) != 0 || !slices.Equal(saved.CookiePool.TargetAccountIDs, []int64{target.ID()}) {
					t.Fatalf("wrong source cleanup: %+v", saved)
				}
			} else if len(saved.CookiePool.TargetAccountIDs) != 0 || len(saved.WSSession.AccountIDs) != 0 || !slices.Equal(saved.CookiePool.SourceAccountIDs, []int64{source.ID()}) {
				t.Fatalf("wrong target/WS cleanup: %+v", saved)
			}
			if saved.Revision == v.Revision || gatewayRouteRevision(saved) == gatewayRouteRevision(v) {
				t.Fatal("cleanup failed to invalidate the old settings and routes")
			}
			// Read through a new manager to prove the repaired configuration was persisted.
			other := &CodexHarvestManager{DB: m.DB, Store: m.Store, Repository: m.Repository, Controls: m.Controls}
			persisted, err := other.GatewaySettings(t.Context())
			if err != nil || persisted.CookiePool.Enabled || persisted.WSSession.Enabled || persisted.Revision != saved.Revision {
				t.Fatalf("invalid persisted settings: %+v %v", persisted, err)
			}
			if _, err = m.SaveGatewaySettings(t.Context(), v); !errors.Is(err, ErrGatewayRevisionConflict) {
				t.Fatalf("stale recovery draft must still conflict: %v", err)
			}
		})
	}
}

func TestCodexGatewaySaveKeepsSurvivingAccounts(t *testing.T) {
	m, source, target, v := newGatewayRegressionFixture(t)
	g := m.gatewayManager()
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
	const deletedID int64 = 13245
	v.CookiePool.TargetAccountIDs = append(v.CookiePool.TargetAccountIDs, deletedID)
	v.WSSession.Enabled = true
	v.WSSession.AccountIDs = []int64{target.ID(), deletedID}
	seedGatewayRegressionSettings(t, m, v)
	v.CookiePool.TTLSeconds = 240
	saved, err := m.SaveGatewaySettings(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.CookiePool.Enabled || !saved.WSSession.Enabled || !slices.Equal(saved.CookiePool.SourceAccountIDs, []int64{source.ID()}) || !slices.Equal(saved.CookiePool.TargetAccountIDs, []int64{target.ID()}) || !slices.Equal(saved.WSSession.AccountIDs, []int64{target.ID()}) {
		t.Fatalf("cleanup changed live selections: %+v", saved)
	}
}

func TestCodexGatewaySaveRejectsNewUnknownAccounts(t *testing.T) {
	m, _, _, v := newGatewayRegressionFixture(t)
	v.CookiePool.TargetAccountIDs = append(v.CookiePool.TargetAccountIDs, 999999)
	if _, err := m.SaveGatewaySettings(t.Context(), v); err == nil {
		t.Fatal("new unknown account was silently accepted")
	}
	current, err := m.GatewaySettings(t.Context())
	if err != nil || slices.Contains(current.CookiePool.TargetAccountIDs, 999999) {
		t.Fatalf("failed save changed configuration: %+v %v", current, err)
	}
}

func TestCodexGatewaySaveDeletedAccountStillValidatesInput(t *testing.T) {
	m, source, _, v := newGatewayRegressionFixture(t)
	m.Store.RemoveAccount(source.ID())
	v.CookiePool.SourceAccountIDs = append(v.CookiePool.SourceAccountIDs, source.ID())
	if _, err := m.SaveGatewaySettings(t.Context(), v); err == nil {
		t.Fatal("duplicate stale IDs bypassed validation")
	}
	v = harvest.DefaultGatewaySettings()
	v.CookiePool.Enabled = true
	if _, err := m.SaveGatewaySettings(t.Context(), v); err == nil {
		t.Fatal("missing selections were accepted as a deletion repair")
	}
}
