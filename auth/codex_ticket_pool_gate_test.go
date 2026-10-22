package auth

import "testing"

func TestCodexTicketGateWithOnlyProxyPool(t *testing.T) {
	previous := ConfiguredCodexTicketSettings()
	pool := CodexTicketProxyPoolConfigured()
	t.Cleanup(func() { SetConfiguredCodexTicketSettings(previous); SetCodexTicketProxyPoolConfigured(pool) })
	settings := DefaultCodexTicketSettings()
	settings.Enabled = true
	settings.HarvestProxyURL = ""
	settings.Models = []string{"gpt-6-astra"}
	SetConfiguredCodexTicketSettings(settings)
	SetCodexTicketProxyPoolConfigured(true)
	if !CodexTicketGateEnabled() {
		t.Fatal("selected proxy pool alone must enable fail-closed ticket gating")
	}
	settings.Enabled = false
	SetConfiguredCodexTicketSettings(settings)
	if CodexTicketGateEnabled() {
		t.Fatal("pool must not enable a disabled policy")
	}
	settings.Enabled = true
	SetConfiguredCodexTicketSettings(settings)
	SetCodexTicketProxyPoolConfigured(false)
	if CodexTicketGateEnabled() {
		t.Fatal("no acquisition exit configured")
	}
}
