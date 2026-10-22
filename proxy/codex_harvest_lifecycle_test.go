package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/tidwall/gjson"
)

func configureHarvestPoolTest(t *testing.T, m *CodexHarvestManager, length int, models []string, proxies ...string) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(proxies))
	for _, proxy := range proxies {
		id, err := m.DB.InsertProxy(t.Context(), proxy, "private-proxy-label")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	settings := auth.ConfiguredCodexTicketSettings()
	settings.TargetLength, settings.Models = length, models
	auth.SetConfiguredCodexTicketSettings(settings)
	controls, _, err := m.Controls.Controls(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	controls.ProxyIDs = ids
	controls.Speed.ProbeIntervalSeconds = 0
	controls.Speed.AttemptTimeoutSeconds = 3
	controls.Speed.CooldownSeconds = 1
	if err = m.SaveControls(t.Context(), controls); err != nil {
		t.Fatal(err)
	}
	old := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = "http://offline-harvest.invalid/backend-api/codex/responses"
	t.Cleanup(func() { codexTicketProbeURLForTest = old })
	return ids
}

func waitHarvestJobTest(t *testing.T, m *CodexHarvestManager, request harvest.ManualRequest) harvest.ManualJob {
	t.Helper()
	job, err := m.StartManual(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, current := range m.Jobs() {
			if current.ID == job.ID && !current.Running {
				return current
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.CancelJob(job.ID)
	t.Fatal("manual harvest job did not finish")
	return job
}

func harvestSuccessFixture(state string, length int) http.HandlerFunc {
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		model := gjson.GetBytes(body, "model").String()
		w.Header().Set(codexTurnStateHeader, state)
		w.Header().Set("Content-Type", "text/event-stream")
		if length == 780 {
			w.Header().Add("Set-Cookie", "__cflb=a; Path=/")
			w.Header().Add("Set-Cookie", "__oailb=h."+claims+".s; Path=/")
		} else {
			w.Header().Add("Set-Cookie", "ticket=paired; Path=/")
		}
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":%q}}\n\n", model)
	}
}

// Converted from the offline alignment audit: assert the real HTTP proxy used,
// not merely a settings round trip.
func TestCodexHarvestProxyIDsSelectActualAutomaticExit(t *testing.T) {
	m, _ := newHarvestManagerTest(t)
	var selectedCalls, fallbackCalls atomic.Int32
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { selectedCalls.Add(1); w.WriteHeader(503) }))
	defer selected.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fallbackCalls.Add(1); w.WriteHeader(503) }))
	defer fallback.Close()
	settings := auth.ConfiguredCodexTicketSettings()
	settings.HarvestProxyURL = fallback.URL
	auth.SetConfiguredCodexTicketSettings(settings)
	configureHarvestPoolTest(t, m, 292, []string{gatewayModel}, selected.URL)
	controls, _, _ := m.Controls.Controls(t.Context())
	controls.Speed.MaxRequestsPerRound, controls.Speed.MaxNodeAttempts = 1, 1
	if err := m.SaveControls(t.Context(), controls); err != nil {
		t.Fatal(err)
	}
	m.RunRound(t.Context())
	if selectedCalls.Load() != 1 || fallbackCalls.Load() != 0 {
		t.Fatalf("selected=%d fallback=%d", selectedCalls.Load(), fallbackCalls.Load())
	}
}

func TestCodexHarvestPoolManualRotationStoresConcreteExit(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	var failed, good atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { failed.Add(1); w.WriteHeader(503) }))
	defer first.Close()
	fixture := harvestSuccessFixture(testTicketState(time.Now(), 10), 292)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { good.Add(1); fixture(w, r) }))
	defer second.Close()
	configureHarvestPoolTest(t, m, 292, []string{gatewayModel}, first.URL, second.URL)
	job := waitHarvestJobTest(t, m, harvest.ManualRequest{AccountID: account.ID(), Models: []string{gatewayModel}, MaxAttempts: 4, RateLimitCooldownSeconds: 1, NodeSwitchRule: "every_request"})
	if job.TicketsStored != 1 || failed.Load() != 1 || good.Load() != 1 {
		t.Fatalf("job=%+v first=%d second=%d", job, failed.Load(), good.Load())
	}
	row, err := m.DB.GetAccountByID(t.Context(), account.ID())
	if err != nil {
		t.Fatal(err)
	}
	ticket := auth.ParseCodexTicket(gatewayModel, row.Credentials[auth.CodexTicketCredentialKey(gatewayModel)])
	if ticket == nil || ticket.HarvestProxyURL != second.URL || ticket.ManagedNode || ticket.HarvestNodeID != "" {
		t.Fatal("concrete pool exit was not persisted independently of Mihomo")
	}
	events, err := m.Repository.Events(t.Context(), account.ID(), job.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), first.URL) || strings.Contains(string(raw), second.URL) || strings.Contains(string(raw), "private-proxy-label") {
		t.Fatal("proxy details leaked to events")
	}
}

func TestCodexHarvestPoolValidationAndRuntimeEligibility(t *testing.T) {
	m, _ := newHarvestManagerTest(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	ids := configureHarvestPoolTest(t, m, 292, []string{gatewayModel}, server.URL)
	base, _, _ := m.Controls.Controls(t.Context())
	for _, values := range [][]int64{{0}, {-1}, {ids[0], ids[0]}, {ids[0] + 99999}} {
		v := base
		v.ProxyIDs = values
		if err := m.SaveControls(t.Context(), v); err == nil {
			t.Fatalf("invalid proxy IDs accepted: %v", values)
		}
	}
	bad, err := m.DB.InsertProxy(t.Context(), "ftp://user:private-password@invalid.example:80", "private")
	if err != nil {
		t.Fatal(err)
	}
	v := base
	v.ProxyIDs = []int64{bad}
	if err = m.SaveControls(t.Context(), v); err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatal("invalid proxy accepted or credentials disclosed")
	}
	no := false
	if err = m.DB.UpdateProxy(t.Context(), ids[0], nil, nil, &no); err != nil {
		t.Fatal(err)
	}
	if err = m.SaveControls(t.Context(), base); err == nil {
		t.Fatal("disabled proxy accepted on save")
	}
	m.RunRound(t.Context())
	if requests.Load() != 0 {
		t.Fatal("disabled pool fell back or issued a request")
	}
	stored, _, _ := m.Controls.Controls(t.Context())
	if !reflect.DeepEqual(stored, base) {
		t.Fatal("failed validation changed persisted configuration")
	}
	yes := true
	if err = m.DB.UpdateProxy(t.Context(), ids[0], nil, nil, &yes); err != nil {
		t.Fatal(err)
	}
	if err = m.DB.UpdateProxyTestResult(t.Context(), ids[0], server.URL, "error", "", "", 0); err != nil {
		t.Fatal(err)
	}
	if err = m.SaveControls(t.Context(), base); err == nil {
		t.Fatal("failed proxy accepted on save")
	}
}

func TestCodexHarvestParallelIdentityChangeCancelsBeforeSendAndStore(t *testing.T) {
	for _, changeAt := range []int32{1, 2} {
		t.Run(fmt.Sprint(changeAt), func(t *testing.T) {
			m, account := newHarvestManagerTest(t)
			var requests atomic.Int32
			fixture := harvestSuccessFixture(testTicketState(time.Now(), 33), 780)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == changeAt {
					m.Store.ApplyAccountCustomHeaders(account.ID(), map[string]string{"ChatGPT-Account-ID": "replacement-workspace"})
				}
				fixture(w, r)
			}))
			defer server.Close()
			configureHarvestPoolTest(t, m, 780, []string{gatewayModel}, server.URL)
			job := waitHarvestJobTest(t, m, harvest.ManualRequest{AccountID: account.ID(), Models: []string{gatewayModel}, CollectLanes: 2, MaxAttempts: 8, RateLimitCooldownSeconds: 1, NodeSwitchRule: "every_request"})
			if requests.Load() != changeAt || job.TicketsStored != 0 || account.CodexTicketSnapshot(gatewayModel) != nil {
				t.Fatalf("old identity published: requests=%d job=%+v", requests.Load(), job)
			}
			row, err := m.DB.GetAccountByID(t.Context(), account.ID())
			if err != nil {
				t.Fatal(err)
			}
			if row.Credentials[auth.CodexTicketCredentialKey(gatewayModel)] != nil {
				t.Fatal("old identity persisted")
			}
		})
	}
}

func TestCodexHarvestParallelRetryAfterPersistsLongestCooldown(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "600"); w.WriteHeader(429) }))
	defer server.Close()
	configureHarvestPoolTest(t, m, 780, []string{gatewayModel}, server.URL)
	job := waitHarvestJobTest(t, m, harvest.ManualRequest{AccountID: account.ID(), Models: []string{gatewayModel}, CollectLanes: 8, MaxAttempts: 20, RateLimitCooldownSeconds: 2, NodeSwitchRule: "every_request"})
	if job.Attempts != 1 || job.LastEvent.RetryAfterSeconds != 600 {
		t.Fatalf("retry feedback lost: %+v", job)
	}
	row, err := m.DB.GetAccountByID(t.Context(), account.ID())
	if err != nil {
		t.Fatal(err)
	}
	var probe auth.CodexTicketProbeSummary
	raw, _ := json.Marshal(row.Credentials[auth.CodexTicketProbeCredentialKey(gatewayModel)])
	if err = json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.NextProbeAt == nil || probe.NextProbeAt.Before(time.Now().Add(590*time.Second)) {
		t.Fatalf("Retry-After shortened: %+v", probe)
	}
	short := harvest.Event{Result: "network_error"}
	m.recordHarvestProbe(account, gatewayModel, short, 1, 0)
	account.Mu().RLock()
	until := *account.CodexTicketProbes[gatewayModel].NextProbeAt
	account.Mu().RUnlock()
	if until.Before(*probe.NextProbeAt) {
		t.Fatal("late failure shortened cooldown")
	}
}

func TestCodexHarvestManualTerminalSkipsRejectedModel(t *testing.T) {
	for _, lanes := range []int{1, 8} {
		t.Run(fmt.Sprint(lanes), func(t *testing.T) {
			m, account := newHarvestManagerTest(t)
			var rejected, good atomic.Int32
			fixture := harvestSuccessFixture(testTicketState(time.Now(), 33), 780)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(strings.NewReader(string(body)))
				if gjson.GetBytes(body, "model").String() == gatewayModel {
					rejected.Add(1)
					w.WriteHeader(404)
					return
				}
				good.Add(1)
				fixture(w, r)
			}))
			defer server.Close()
			models := []string{gatewayModel, "gpt-5.6-sol"}
			configureHarvestPoolTest(t, m, 780, models, server.URL)
			job := waitHarvestJobTest(t, m, harvest.ManualRequest{AccountID: account.ID(), Models: models, CollectLanes: lanes, MaxAttempts: 150, RateLimitCooldownSeconds: 1, NodeSwitchRule: "every_request"})
			if rejected.Load() != 1 || good.Load() != 2 || job.TicketsStored != 1 || job.Attempts != 3 {
				t.Fatalf("terminal retry consumed budget: rejected=%d good=%d job=%+v", rejected.Load(), good.Load(), job)
			}
		})
	}
}

func TestCodexHarvestPoolParallelLargeRequestUsesBoundedLanesAndBudget(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	var requests, active, peak atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		requests.Add(1)
		w.WriteHeader(503)
	})
	first := httptest.NewServer(handler)
	defer first.Close()
	second := httptest.NewServer(handler)
	defer second.Close()
	configureHarvestPoolTest(t, m, 292, []string{gatewayModel}, first.URL, second.URL)
	job := waitHarvestJobTest(t, m, harvest.ManualRequest{AccountID: account.ID(), Models: []string{gatewayModel}, CollectLanes: 1 << 20, MaxAttempts: 110, RateLimitCooldownSeconds: 1, NodeSwitchRule: "every_request"})
	if job.Attempts != 110 || requests.Load() != 110 || peak.Load() > 2 {
		t.Fatalf("unbounded workers or wrong budget: calls=%d peak=%d job=%+v", requests.Load(), peak.Load(), job)
	}
}
