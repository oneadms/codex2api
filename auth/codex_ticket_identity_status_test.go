package auth

import (
	"testing"
	"time"
)

func TestCodexTicketStatusesRejectOldIdentityAndAllowMatchingStandby(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	account := &Account{AccountID: "workspace-old", Email: "owner@example.invalid"}
	ticket := &CodexTicket{Model: "gpt-6-astra", State: buildTestTicketState(now, CodexTicketPersonalBlocks), Cookie: "ticket=old", Length: 292, CapturedAt: now, ExpiresAt: now.Add(time.Minute), Identity: account.CodexTicketIdentitySnapshot()}
	account.CodexTickets = map[string]*CodexTicket{ticket.Model: ticket}
	if !account.CodexTicketStatuses([]string{ticket.Model}, now, 292)[0].Ready {
		t.Fatal("fixture must start ready")
	}
	account.mu.Lock()
	account.CustomHeaders = map[string]string{"ChatGPT-Account-ID": "workspace-new"}
	account.mu.Unlock()
	status := account.CodexTicketStatuses([]string{ticket.Model}, now, 292)[0]
	if status.Ready || status.StandbyReady || account.CodexTicketForModel(ticket.Model, now, 292) != nil {
		t.Fatal("old identity remained ready")
	}
	standby := *ticket
	standby.Identity = account.CodexTicketIdentitySnapshot()
	standby.Cookie = "ticket=new"
	account.mu.Lock()
	ticket.Standby = &standby
	account.mu.Unlock()
	status = account.CodexTicketStatuses([]string{ticket.Model}, now, 292)[0]
	selected := account.CodexTicketForModel(ticket.Model, now, 292)
	if !status.Ready || status.StandbyReady || selected == nil || selected.Cookie != "ticket=new" {
		t.Fatal("matching standby not reflected consistently")
	}
	account.mu.Lock()
	account.Email = "different@example.invalid"
	account.mu.Unlock()
	if account.CodexTicketStatuses([]string{ticket.Model}, now, 292)[0].Ready {
		t.Fatal("email change retained ready state")
	}
}
