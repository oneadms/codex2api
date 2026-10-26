package admin

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestTraeCNModelSyncSlotHonorsTimeAndTimezone(t *testing.T) {
	settings := auth.TraeCNSettings{ModelSyncEnabled: true, ModelSyncTime: "00:00", ModelSyncTimezone: "Asia/Shanghai"}
	parse := func(raw string) time.Time {
		v, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	now := parse("2026-10-26T16:00:00Z") // Beijing midnight on October 27.
	slot, due := traeCNModelSyncSlot(settings, now)
	if !due || !slot.Equal(now) || slot.Day() != 27 {
		t.Fatalf("midnight slot=%s due=%v", slot, due)
	}
	settings.ModelSyncTime = "03:25"
	for _, tc := range []struct {
		now string
		due bool
	}{
		{"2026-10-26T19:24:59Z", false},
		{"2026-10-26T19:25:00Z", true},
		{"2026-10-27T08:00:00Z", true},  // Catch up a missed run today.
		{"2026-10-27T16:00:00Z", false}, // Next local day, before its time.
	} {
		if _, due := traeCNModelSyncSlot(settings, parse(tc.now)); due != tc.due {
			t.Errorf("time=%s due=%v want=%v", tc.now, due, tc.due)
		}
	}
	settings.ModelSyncTimezone = "UTC"
	if slot, due := traeCNModelSyncSlot(settings, parse("2026-10-27T03:25:00Z")); !due || slot.Hour() != 3 || slot.Minute() != 25 {
		t.Fatalf("UTC schedule: %s due=%v", slot, due)
	}
	settings.ModelSyncEnabled = false
	if _, due := traeCNModelSyncSlot(settings, now); due {
		t.Fatal("disabled schedule was due")
	}
	settings.ModelSyncEnabled = true
	settings.ModelSyncTimezone = "Invalid/Timezone"
	if _, due := traeCNModelSyncSlot(settings, now); due {
		t.Fatal("invalid timezone was due")
	}
}

func TestTraeCNModelSyncSchedulerPersistsSuccessAndBoundsFailureRetries(t *testing.T) {
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	settings := auth.TraeCNSettings{ModelSyncEnabled: false, ModelSyncTime: "00:00", ModelSyncTimezone: "UTC"}
	auth.SetConfiguredTraeCNSettings(settings)
	h, insert, calls := newTraeCNModelSyncTestHandler(t)
	good := insert("good", auth.UpstreamTraeCN, true)
	insert("lazy", auth.UpstreamTraeCN, false)
	insert("fail", auth.UpstreamTraeCN, true)
	disabled := insert("disabled", auth.UpstreamTraeCN, true)
	deleted := insert("deleted", auth.UpstreamTraeCN, true)
	insert("other", "codex", true)
	if err := h.db.SetAccountEnabled(t.Context(), disabled, false); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SoftDeleteAccount(t.Context(), deleted); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	h.runTraeCNModelSyncDue(t.Context(), midnight)
	if calls("good") != 0 {
		t.Fatal("disabled scheduler fetched models")
	}
	settings.ModelSyncEnabled = true
	auth.SetConfiguredTraeCNSettings(settings)
	h.runTraeCNModelSyncDue(t.Context(), midnight)
	if calls("good") != 1 || calls("lazy") != 1 || calls("fail") != 1 || calls("disabled") != 0 || calls("deleted") != 0 || calls("other") != 0 {
		t.Fatalf("scheduler scope: good=%d lazy=%d fail=%d disabled=%d deleted=%d other=%d", calls("good"), calls("lazy"), calls("fail"), calls("disabled"), calls("deleted"), calls("other"))
	}
	// Minute polls cannot spam an unavailable provider. Successful accounts
	// are skipped, failures retry after ten minutes with a three-attempt cap.
	for _, elapsed := range []time.Duration{time.Minute, 9 * time.Minute, 11 * time.Minute, 12 * time.Minute, 22 * time.Minute, 33 * time.Minute} {
		h.runTraeCNModelSyncDue(t.Context(), midnight.Add(elapsed))
	}
	if calls("good") != 1 || calls("lazy") != 1 || calls("fail") != 3 {
		t.Fatalf("retry budget violated: good=%d lazy=%d fail=%d", calls("good"), calls("lazy"), calls("fail"))
	}
	// Re-create the runtime and handler like a restart. Persisted timestamps
	// prevent repeating successful scheduled syncs, including manual syncs.
	if err := h.store.LoadAccountByID(t.Context(), good); err != nil {
		t.Fatal(err)
	}
	restarted := &Handler{db: h.db, store: h.store}
	restarted.runTraeCNModelSyncDue(t.Context(), midnight.Add(40*time.Minute))
	if calls("good") != 1 || calls("lazy") != 1 {
		t.Fatal("restart repeated successful daily syncs")
	}
	// Turning it off takes effect without restarting the scheduler.
	settings.ModelSyncEnabled = false
	auth.SetConfiguredTraeCNSettings(settings)
	before := calls("fail")
	h.runTraeCNModelSyncDue(t.Context(), midnight.AddDate(0, 0, 1))
	if calls("fail") != before {
		t.Fatal("disabled scheduler kept working")
	}
	settings.ModelSyncEnabled = true
	auth.SetConfiguredTraeCNSettings(settings)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.runTraeCNModelSyncDue(ctx, midnight.AddDate(0, 0, 1))
	if calls("fail") != before {
		t.Fatal("cancelled scan fetched models")
	}
}

func TestTraeCNModelSyncSchedulerStopsOnCancellation(t *testing.T) {
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})
	h, _, _ := newTraeCNModelSyncTestHandler(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.StartTraeCNModelSyncScheduler(ctx)
	h.StartTraeCNModelSyncScheduler(ctx)
	cancel()
	done := make(chan struct{})
	go func() {
		h.WaitTraeCNModelSyncScheduler()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler failed to shut down")
	}
}
