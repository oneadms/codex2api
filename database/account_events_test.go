package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const traeCNCreditsProbeExhaustedType = "traecn_credits_probe_exhausted"
const traeCNCreditsProbeExhaustedSource = "积分仍不足，等待下一次日探针"

func TestTraeCNCreditsProbeEventTypeExceedsLegacyColumn(t *testing.T) {
	if n := len(traeCNCreditsProbeExhaustedType); n <= 20 {
		t.Fatalf("legacy VARCHAR(20) would have stored %q (%d chars)", traeCNCreditsProbeExhaustedType, n)
	}
	if n := len(traeCNCreditsProbeExhaustedType); n > accountEventTypeMaxRunes {
		t.Fatalf("%q is %d runes, wider than VARCHAR(%d)", traeCNCreditsProbeExhaustedType, n, accountEventTypeMaxRunes)
	}
}

func TestInsertAccountEventAcceptsCreditsProbeExhausted(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.InsertAccountEvent(ctx, 13215, traeCNCreditsProbeExhaustedType, traeCNCreditsProbeExhaustedSource); err != nil {
		t.Fatal(err)
	}
	var gotType, gotSource string
	if err := db.conn.QueryRowContext(ctx, `SELECT event_type, source FROM account_events WHERE account_id = 13215`).Scan(&gotType, &gotSource); err != nil {
		t.Fatal(err)
	}
	if gotType != traeCNCreditsProbeExhaustedType || gotSource != traeCNCreditsProbeExhaustedSource {
		t.Fatalf("got type=%q source=%q", gotType, gotSource)
	}
}

func TestClipAccountEventFieldsTruncatesToColumnWidth(t *testing.T) {
	eventType, source := clipAccountEventFields(strings.Repeat("e", accountEventTypeMaxRunes+8), strings.Repeat("源", accountEventSourceMaxRunes+3))
	if n := len([]rune(eventType)); n != accountEventTypeMaxRunes {
		t.Fatalf("event_type runes=%d", n)
	}
	if n := len([]rune(source)); n != accountEventSourceMaxRunes {
		t.Fatalf("source runes=%d", n)
	}
}

func TestPostgresAccountEventTypeColumnHoldsProbeEvents(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var eventLen, sourceLen int
	if err := db.conn.QueryRowContext(ctx, `SELECT character_maximum_length FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'account_events' AND column_name = 'event_type'`).Scan(&eventLen); err != nil {
		t.Fatal(err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT character_maximum_length FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'account_events' AND column_name = 'source'`).Scan(&sourceLen); err != nil {
		t.Fatal(err)
	}
	if eventLen < len(traeCNCreditsProbeExhaustedType) {
		t.Fatalf("event_type VARCHAR(%d) cannot store %q", eventLen, traeCNCreditsProbeExhaustedType)
	}
	if sourceLen < len([]rune(traeCNCreditsProbeExhaustedSource)) {
		t.Fatalf("source VARCHAR(%d) cannot store probe source", sourceLen)
	}
	if err := db.InsertAccountEvent(ctx, 13215, traeCNCreditsProbeExhaustedType, traeCNCreditsProbeExhaustedSource); err != nil {
		t.Fatal(err)
	}
}
