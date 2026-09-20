package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func demandSetup(t *testing.T, handler http.HandlerFunc) pluginConfig {
	t.Helper()
	resetProbeRunner(t)
	cpa := newFakeCPA(t, fakeCredSeed{name: probeTestAccount, accountID: "account-a"}, fakeCredSeed{name: probeTestOther, accountID: "account-b"})
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	setUpstream(t, upstream.URL)
	cfg := probeTestConfig(probeConfigOptions{dir: t.TempDir(), baseURL: cpa.server.URL, role: roleBusiness, mgmtKey: "test", templateLen: 292, replaceLen: 312, ttlSeconds: 3600})
	cfg += fmt.Sprintf("on_demand: true\non_demand_accounts: [%q, %q]\non_demand_timeout_seconds: 2\n", probeTestAccount, probeTestOther)
	mustConfigure(t, cfg)
	t.Cleanup(resetPluginConfig)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.config
}

func demandRequest(account, model string) pluginapi.RequestInterceptRequest {
	req := request(account, model, "")
	req.ToFormat = "codex"
	req.Body = []byte(`{"model":"client-alias","input":"business prompt"}`)
	return req
}

func TestOnDemandWaitsThenReusesOnlyExactAccountModel(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	var mu sync.Mutex
	seen := map[string]string{}
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := count.Add(1)
		if n == 1 {
			close(entered)
			<-release
		}
		value := fakeTokenSeed(292, time.Now().Add(-time.Second), byte(n))
		mu.Lock()
		seen[r.Header.Get("Chatgpt-Account-Id")+"/"+body.Model] = value
		mu.Unlock()
		if r.Header.Get(turnStateHeader) != "" {
			t.Error("probe must not replay inbound ticket")
		}
		w.Header().Set(turnStateHeader, value)
	})
	done := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() { done <- interceptAfter(t, demandRequest(probeTestAccount, "model-a")) }()
	<-entered
	select {
	case <-done:
		t.Fatal("business request released before acquisition finished")
	default:
	}
	close(release)
	first := <-done
	if first.Terminate || len(outgoingHeader(first)) != 292 {
		t.Fatal("valid acquisition failed")
	}
	// A foreign caller-supplied state is ignored; only the exact bucket is used.
	req := demandRequest(probeTestAccount, "model-a")
	req.RequestedModel = "another-alias"
	req.Headers.Set(turnStateHeader, fakeTokenSeed(292, time.Now(), 99))
	second := interceptAfter(t, req)
	if outgoingHeader(first) != outgoingHeader(second) || count.Load() != 1 {
		t.Fatal("cache was not reused")
	}
	third := interceptAfter(t, demandRequest(probeTestOther, "model-a"))
	fourth := interceptAfter(t, demandRequest(probeTestAccount, "model-b(high)"))
	if count.Load() != 3 {
		t.Fatalf("want separate probes for each account/model, got %d", count.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if outgoingHeader(third) != seen["account-b/model-a"] || outgoingHeader(fourth) != seen["account-a/model-b"] || outgoingHeader(first) != seen["account-a/model-a"] {
		t.Fatal("account/model isolation failed")
	}
}

func TestOnDemandConcurrentRequestsShareOneProbe(t *testing.T) {
	var count atomic.Int32
	cfg := demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		time.Sleep(80 * time.Millisecond)
		w.Header().Set(turnStateHeader, fakeToken(292, time.Now().Add(-time.Second)))
	})
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
			if resp.Terminate || len(outgoingHeader(resp)) != 292 {
				t.Error("shared acquisition failed")
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("duplicate acquisitions: %d", count.Load())
	}
	if err := probeRunStart(); err == nil {
		t.Fatal("background probe allowed in on-demand mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	pool := newProbeClientPool()
	defer pool.closeIdle()
	probeRenewLoop(ctx, pool)
	if count.Load() != 1 {
		t.Fatal("idle renewal made an upstream call")
	}
	// Passive responses from an unselected account must not create tickets.
	harvestFromResponse(cfg, http.Header{turnStateHeader: {fakeToken(292, time.Now())}}, map[string]any{selectedAuthMetadataKey: "codex-unselected.json"}, "model-a", "")
	if value := demandCached(cfg, "codex-unselected.json", "model-a"); value != "" {
		t.Fatal("passive harvesting was not disabled")
	}
}

func TestOnDemandRejectsInvalidTicketsAndFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		value  string
	}{
		{"degraded", 200, fakeToken(312, time.Now())},
		{"missing", 200, ""},
		{"malformed", 200, strings.Repeat("x", 292)},
		{"expired", 200, fakeToken(292, time.Now().Add(-time.Hour))},
		{"near-expiry", 200, fakeToken(292, time.Now().Add(-time.Hour+3*time.Second))},
		{"future", 200, fakeToken(292, time.Now().Add(time.Minute))},
		{"rate-limited", 429, ""}, {"unauthorized", 401, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set(turnStateHeader, tc.value)
				w.WriteHeader(tc.status)
			})
			resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
			if !resp.Terminate || resp.StatusCode != 503 || len(resp.Headers) != 0 {
				t.Fatal("business request was allowed without a valid ticket")
			}
			if calls.Load() != 1 {
				t.Fatal("unexpected probe count")
			}
			if !json.Valid(resp.ResponseBody) {
				t.Fatal("invalid client error")
			}
		})
	}
}

func TestOnDemandExpiredCacheTriggersAcquisition(t *testing.T) {
	var calls atomic.Int32
	cfg := demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set(turnStateHeader, fakeToken(292, time.Now()))
	})
	// Even a freshly saved record cannot extend the ticket's embedded timestamp.
	rec := storeRecordFor(probeTestAccount, "model-a", time.Now(), 292)
	rec.Value = fakeToken(292, time.Now().Add(-2*time.Hour))
	mustWriteRecord(t, cfg.StoreDir, rec)
	resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
	if resp.Terminate || calls.Load() != 1 {
		t.Fatal("expired embedded timestamp was reused")
	}
}

func TestOnDemandScopeMetadataAndRuntimeGuards(t *testing.T) {
	var calls atomic.Int32
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, req := range []pluginapi.RequestInterceptRequest{demandRequest("", "model-a"), demandRequest(probeTestAccount, "")} {
		if resp := interceptAfter(t, req); !resp.Terminate {
			t.Fatal("missing identity was guessed")
		}
	}
	req := demandRequest("codex-unselected.json", "model-a")
	if resp := interceptAfter(t, req); resp.Terminate || len(resp.Headers) != 0 {
		t.Fatal("unselected account modified")
	}
	req = demandRequest(probeTestAccount, "model-a")
	req.Headers.Set("Upgrade", "websocket")
	if resp := interceptAfter(t, req); !resp.Terminate || resp.StatusCode != 400 {
		t.Fatal("WebSocket allowed without per-request ticket guarantee")
	}
	if handleDryRunResource(url.Values{"value": {"true"}}).StatusCode != 409 {
		t.Fatal("dry-run bypass allowed")
	}
	if handleRoleResource(url.Values{"value": {"probe"}}).StatusCode != 409 {
		t.Fatal("probe role bypass allowed")
	}
	if calls.Load() != 0 {
		t.Fatal("guard triggered acquisition")
	}
}

func TestOnDemandModelWhitelistBypassesExcludedModels(t *testing.T) {
	var calls atomic.Int32
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	state.mu.Lock()
	state.config.OnDemandModels = []string{"gpt-5.6-sol"}
	state.mu.Unlock()

	for _, model := range []string{"codex-auto-review", "gpt-5.6-luna"} {
		resp := interceptAfter(t, demandRequest(probeTestAccount, model))
		if resp.Terminate || len(resp.Headers) != 0 {
			t.Fatalf("excluded model %q was gated: %+v", model, resp)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("excluded models triggered %d acquisition calls", calls.Load())
	}
}

func TestOnDemandTimeoutCancelsProbe(t *testing.T) {
	canceled := make(chan struct{})
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(4 * time.Second):
		}
	})
	state.mu.Lock()
	state.config.OnDemandTimeoutSeconds = 1
	state.mu.Unlock()
	start := time.Now()
	resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
	if !resp.Terminate || resp.StatusCode != 504 || time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not fail closed")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("probe continued after timeout")
	}
	demandWork.Lock()
	defer demandWork.Unlock()
	if len(demandWork.flights) != 0 || len(demandWork.accounts) != 0 {
		t.Fatal("request left a background worker")
	}
}

func TestOnDemandDefaultOffAndConfigValidation(t *testing.T) {
	if defaultConfig().OnDemand {
		t.Fatal("must be opt-in")
	}
	mustConfigure(t, businessConfig(t.TempDir(), false))
	if resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a")); resp.Terminate || len(resp.Headers) != 0 {
		t.Fatal("default behavior changed")
	}
	cfg := defaultConfig()
	cfg.OnDemand = true
	if err := validateDemandConfig(cfg); err == nil {
		t.Fatal("incomplete enabled configuration accepted")
	}
}

func TestOnDemandReturnsAfterHeadersWithoutWaitingForSSEBody(t *testing.T) {
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set(turnStateHeader, fakeToken(292, time.Now()))
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	start := time.Now()
	resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
	if resp.Terminate || time.Since(start) > time.Second {
		t.Fatal("waited for SSE body instead of using headers")
	}
}

func TestOnDemandConfigChangeDuringAcquisitionRejectsStaleResult(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	cfg := demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set(turnStateHeader, fakeToken(292, time.Now()))
	})
	done := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() { done <- interceptAfter(t, demandRequest(probeTestAccount, "model-a")) }()
	<-entered
	state.mu.Lock()
	state.config.OnDemandAccounts = []string{probeTestOther}
	state.mu.Unlock()
	close(release)
	if resp := <-done; !resp.Terminate {
		t.Fatal("released request after its account was removed")
	}
	records, err := scanStoreRecords(cfg.StoreDir)
	if err != nil || len(records) != 0 {
		t.Fatal("stale acquisition wrote a ticket after configuration change")
	}
}

func TestOnDemandReusesPersistedTicketWithoutProbing(t *testing.T) {
	var calls atomic.Int32
	cfg := demandSetup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	rec := storeRecordFor(probeTestAccount, "model-a", time.Now().Add(-time.Minute), 292)
	mustWriteRecord(t, cfg.StoreDir, rec)
	resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
	if resp.Terminate || outgoingHeader(resp) != rec.Value || calls.Load() != 0 {
		t.Fatal("persisted valid ticket was not reused")
	}
}

func resetTicketActivitiesForTest(t *testing.T) {
	t.Helper()
	ticketActivities.Lock()
	old := ticketActivities.items
	ticketActivities.items = nil
	ticketActivities.Unlock()
	t.Cleanup(func() {
		ticketActivities.Lock()
		ticketActivities.items = old
		ticketActivities.Unlock()
	})
}

func TestManualTicketAcquireReportsAcquiredThenReused(t *testing.T) {
	resetTicketActivitiesForTest(t)
	var calls atomic.Int32
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set(turnStateHeader, fakeToken(292, time.Now()))
	})
	query := url.Values{"account": {probeTestAccount}, "model": {"model-a"}}

	first := handleTicketAcquireResource(query)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("manual acquisition returned %d: %s", first.StatusCode, first.Body)
	}
	var acquired ticketAcquireResponse
	if err := json.Unmarshal(first.Body, &acquired); err != nil {
		t.Fatalf("decode acquisition: %v", err)
	}
	if !acquired.Success || acquired.Result != "acquired" || acquired.SecondsLeft <= 0 || acquired.ExpiresAt == "" || acquired.Attempts != 1 || acquired.Returned292 != 1 || acquired.OutcomeSummary != "本次打票尝试 1 次，292×1，312×0" {
		t.Fatalf("unexpected acquisition response: %+v", acquired)
	}
	if calls.Load() != 1 {
		t.Fatalf("manual acquisition made %d upstream calls, want 1", calls.Load())
	}

	second := handleTicketAcquireResource(query)
	var reused ticketAcquireResponse
	if second.StatusCode != http.StatusOK || json.Unmarshal(second.Body, &reused) != nil {
		t.Fatalf("manual reuse returned %d: %s", second.StatusCode, second.Body)
	}
	if reused.Result != "reused" || reused.Attempts != 0 || calls.Load() != 1 {
		t.Fatalf("valid ticket was not reused: response=%+v calls=%d", reused, calls.Load())
	}
	activity := ticketActivitySnapshot()
	if len(activity) != 2 || activity[0].Kind != "manual" || activity[0].Result != "reused" || activity[1].Result != "acquired" {
		t.Fatalf("unexpected activity: %+v", activity)
	}
}

func TestManualTicketAcquireReportsFailure(t *testing.T) {
	resetTicketActivitiesForTest(t)
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(turnStateHeader, fakeToken(312, time.Now()))
	})
	resp := handleTicketAcquireResource(url.Values{"account": {probeTestAccount}, "model": {"model-a"}})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("manual failure returned %d: %s", resp.StatusCode, resp.Body)
	}
	activity := ticketActivitySnapshot()
	if len(activity) != 1 || activity[0].Kind != "manual" || activity[0].Result != "failed" || !strings.Contains(activity[0].Message, "312×1") {
		t.Fatalf("manual failure not recorded: %+v", activity)
	}
}

func TestProbeAttemptBudgetStopsAtManualForceLimitAndCounts312(t *testing.T) {
	ctx, budget := withProbeAttemptCounter(context.Background(), defaultTicketAcquireMaxAttempts)
	for i := int32(0); i < defaultTicketAcquireMaxAttempts; i++ {
		if !probeCountAttempt(ctx) {
			t.Fatalf("attempt %d was rejected before the limit", i+1)
		}
		probeRecordAttemptResponse(ctx, http.StatusOK, strings.Repeat("x", 312))
	}
	if probeCountAttempt(ctx) {
		t.Fatal("attempt beyond the manual force limit was allowed")
	}
	stats := budget.stats()
	if stats.Attempts != defaultTicketAcquireMaxAttempts || stats.Returned312 != defaultTicketAcquireMaxAttempts || stats.Returned292 != 0 {
		t.Fatalf("unexpected attempt stats: %+v", stats)
	}
	want := "本次打票尝试 10 次，292×0，312×10"
	if got := formatAttemptStats(stats); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestManualTicketSeparates401FromCooldownAndAllowsForcedRetry(t *testing.T) {
	resetTicketActivitiesForTest(t)
	var calls atomic.Int32
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	query := url.Values{"account": {probeTestAccount}, "model": {"model-a"}}

	first := handleTicketAcquireResource(query)
	if first.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(first.Body), "401") || !strings.Contains(string(first.Body), "重新授权") {
		t.Fatalf("401 was not reported as reauthorization: status=%d body=%s", first.StatusCode, first.Body)
	}
	block, blocked := probeAccountBlock(probeTestAccount, time.Now())
	if !blocked || block.Status != "reauthorization_required" || block.SecondsLeft != 0 {
		t.Fatalf("401 block = %+v, %v; want reauthorization without cooldown", block, blocked)
	}

	second := handleTicketAcquireResource(query)
	if second.StatusCode != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("ordinary retry should be locally blocked: status=%d calls=%d", second.StatusCode, calls.Load())
	}
	query.Set("force", "1")
	forced := handleTicketAcquireResource(query)
	if forced.StatusCode != http.StatusServiceUnavailable || calls.Load() != 2 {
		t.Fatalf("forced retry did not reach upstream: status=%d calls=%d body=%s", forced.StatusCode, calls.Load(), forced.Body)
	}
}

func TestManualTicket403CoolsDownAndForcedRetryCanRecover(t *testing.T) {
	resetTicketActivitiesForTest(t)
	var calls atomic.Int32
	demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set(turnStateHeader, fakeToken(292, time.Now().Add(-time.Second)))
	})
	query := url.Values{"account": {probeTestAccount}, "model": {"model-a"}}

	first := handleTicketAcquireResource(query)
	block, blocked := probeAccountBlock(probeTestAccount, time.Now())
	if first.StatusCode != http.StatusServiceUnavailable || !blocked || block.Status != "cooldown" || block.SecondsLeft <= 0 || !strings.Contains(block.Reason, "403") {
		t.Fatalf("403 cooldown mismatch: status=%d body=%s block=%+v blocked=%v", first.StatusCode, first.Body, block, blocked)
	}
	second := handleTicketAcquireResource(query)
	if second.StatusCode != http.StatusServiceUnavailable || calls.Load() != 1 || !strings.Contains(string(second.Body), "强制打票") {
		t.Fatalf("ordinary retry should explain local cooldown: status=%d calls=%d body=%s", second.StatusCode, calls.Load(), second.Body)
	}

	query.Set("force", "true")
	forced := handleTicketAcquireResource(query)
	if forced.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("forced retry failed: status=%d calls=%d body=%s", forced.StatusCode, calls.Load(), forced.Body)
	}
	var result ticketAcquireResponse
	if err := json.Unmarshal(forced.Body, &result); err != nil || !result.Success || !result.Forced || result.Result != "acquired" || result.Attempts != 1 {
		t.Fatalf("unexpected forced response: %+v err=%v", result, err)
	}
	if _, blocked := probeAccountBlock(probeTestAccount, time.Now()); blocked {
		t.Fatal("successful forced acquisition did not clear account cooldown")
	}
}
