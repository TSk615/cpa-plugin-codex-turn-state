package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeTeamToken is a wholly synthetic 249-byte Fernet-shaped value. Padded
// base64 encodes it to the 332 characters used by Team turn-state tickets.
func fakeTeamToken(issued time.Time, seed byte) string {
	raw := make([]byte, 249)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	for i := 9; i < len(raw); i++ {
		raw[i] = seed + byte(i)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func planDemandSetup(t *testing.T, handler http.HandlerFunc, firstPlan, secondPlan string) pluginConfig {
	t.Helper()
	resetProbeRunner(t)
	cpa := newFakeCPA(t,
		fakeCredSeed{name: probeTestAccount, accountID: "account-a", planType: firstPlan},
		fakeCredSeed{name: probeTestOther, accountID: "account-b", planType: secondPlan},
	)
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	setUpstream(t, upstream.URL)
	cfg := probeTestConfig(probeConfigOptions{
		dir: t.TempDir(), baseURL: cpa.server.URL, role: roleBusiness,
		mgmtKey: "test", templateLen: 292, replaceLen: 312, ttlSeconds: 3600,
	})
	cfg += fmt.Sprintf("on_demand: true\non_demand_accounts: [%q, %q]\non_demand_timeout_seconds: 2\n", probeTestAccount, probeTestOther)
	mustConfigure(t, cfg)
	t.Cleanup(resetPluginConfig)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.config
}

func teamDemandSetup(t *testing.T, handler http.HandlerFunc) pluginConfig {
	t.Helper()
	return planDemandSetup(t, handler, "team", "business")
}

func TestTeam332OnDemandAcquiresReusesAndKeepsBucketsSeparate(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	issued := time.Now().Add(-time.Second)
	seen := make(map[string]string)
	teamDemandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe request: %v", err)
			return
		}
		n := calls.Add(1)
		key := r.Header.Get("Chatgpt-Account-Id") + "/" + body.Model
		value := fakeTeamToken(issued, byte(n))
		mu.Lock()
		seen[key] = value
		mu.Unlock()
		w.Header().Set(turnStateHeader, value)
	})

	first := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
	second := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
	otherAccount := interceptAfter(t, demandRequest(probeTestOther, "model-a"))
	otherModel := interceptAfter(t, demandRequest(probeTestAccount, "model-b"))
	if first.Terminate || second.Terminate || otherAccount.Terminate || otherModel.Terminate {
		t.Fatal("a valid Team ticket was rejected")
	}
	if calls.Load() != 3 {
		t.Fatalf("probe calls = %d, want 3 (one cached reuse and two distinct buckets)", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if outgoingHeader(first) != seen["account-a/model-a"] || outgoingHeader(second) != outgoingHeader(first) {
		t.Fatal("Team ticket was not injected and reused for its exact bucket")
	}
	if outgoingHeader(otherAccount) != seen["account-b/model-a"] || outgoingHeader(otherModel) != seen["account-a/model-b"] {
		t.Fatal("Team tickets crossed an account or model boundary")
	}
}

func TestTeam332DefaultStoreLoadsAndDemandReusesIt(t *testing.T) {
	var calls atomic.Int32
	cfg := teamDemandSetup(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	issued := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	rec := storeRecord{
		AuthID: probeTestAccount, Model: "model-a", Len: 332,
		Value: fakeTeamToken(issued, 0x42), IssuedAt: issued.Format(time.RFC3339), HarvestedAt: issued.Format(time.RFC3339), PlanType: "team",
	}
	if err := writeStoreRecord(cfg.StoreDir, rec, 292); err != nil {
		t.Fatalf("write Team record with default template length: %v", err)
	}
	loaded, err := loadStore(cfg.StoreDir, time.Now(), testTTL, 292)
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	if got := loaded[bucketKey(rec.AuthID, rec.Model)].value; got != rec.Value {
		t.Fatal("default 292 configuration did not load a Team 332 ticket")
	}
	// Prove the request path can recover the Team ticket from disk rather than
	// accidentally succeeding through state left behind by another code path.
	state.mu.Lock()
	state.buckets = make(map[string]templateEntry)
	state.store = nil
	state.storeMod = time.Time{}
	state.storeChecked = time.Time{}
	state.mu.Unlock()
	for i := 0; i < 2; i++ {
		resp := interceptAfter(t, demandRequest(rec.AuthID, rec.Model))
		if resp.Terminate || outgoingHeader(resp) != rec.Value {
			t.Fatal("stored Team ticket was not reused")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("cached Team ticket triggered %d probe calls", calls.Load())
	}
}

func TestTeam332OnDemandRejectsExpiredFutureAnd312(t *testing.T) {
	cases := []struct {
		name  string
		value func() string
	}{
		{"expired 332", func() string { return fakeTeamToken(time.Now().Add(-2*time.Hour), 1) }},
		{"future 332", func() string { return fakeTeamToken(time.Now().Add(time.Minute), 2) }},
		{"312", func() string { return fakeTokenSeed(312, time.Now(), 3) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			teamDemandSetup(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set(turnStateHeader, tc.value())
			})
			resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
			if !resp.Terminate || resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatal("invalid ticket allowed the business request")
			}
			if calls.Load() != 1 {
				t.Fatalf("probe calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestOnDemandRejectsTicketLengthForWrongPlan(t *testing.T) {
	cases := []struct {
		name     string
		planType string
		value    func() string
	}{
		{"plus cannot use 332", "plus", func() string { return fakeTeamToken(time.Now().Add(-time.Second), 1) }},
		{"unknown cannot use 332", "", func() string { return fakeTeamToken(time.Now().Add(-time.Second), 2) }},
		{"team cannot use 292", "team", func() string { return fakeTokenSeed(292, time.Now().Add(-time.Second), 3) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			planDemandSetup(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set(turnStateHeader, tc.value())
			}, tc.planType, "business")
			resp := interceptAfter(t, demandRequest(probeTestAccount, "model-a"))
			if !resp.Terminate || resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatal("ticket length for the wrong account plan was accepted")
			}
			if calls.Load() != 1 {
				t.Fatalf("probe calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestLoadStoreEnforcesTicketLengthFromPlanMetadata(t *testing.T) {
	issued := testNow.Add(-time.Minute)
	cases := []struct {
		name     string
		planType string
		length   int
		want     bool
	}{
		{"team 332", "team", 332, true},
		{"business 332", "business", 332, true},
		{"plus 292", "plus", 292, true},
		{"plus 332", "plus", 332, false},
		{"unknown 332", "", 332, false},
		{"team 292", "team", 292, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			value := fakeTeamToken(issued, 0x31)
			if tc.length == 292 {
				value = fakeTokenSeed(292, issued, 0x31)
			}
			rec := storeRecord{
				AuthID: "codex-plan.json", Model: "model-a", Len: tc.length, Value: value,
				IssuedAt: issued.Format(time.RFC3339), HarvestedAt: issued.Format(time.RFC3339), PlanType: tc.planType,
			}
			writeRawRecord(t, dir, rec)
			loaded, err := loadStore(dir, testNow, testTTL, 292)
			if err != nil {
				t.Fatalf("loadStore: %v", err)
			}
			_, got := loaded[bucketKey(rec.AuthID, rec.Model)]
			if got != tc.want {
				t.Fatalf("loaded = %t, want %t for plan %q length %d", got, tc.want, tc.planType, tc.length)
			}
		})
	}
}

func TestCredentialPlanTypePrecedenceAndFallbacks(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	idBusiness := encodeJWT(exp, "account-a", "BuSiNeSs")
	cases := []struct {
		name   string
		claims map[string]any
		blob   map[string]any
		want   string
	}{
		{
			name: "access token auth claim wins",
			claims: map[string]any{"https://api.openai.com/auth": map[string]any{
				"chatgpt_plan_type": "BuSiNeSs",
			}},
			blob: map[string]any{"id_token": encodeJWT(exp, "account-a", "team"), "plan_type": "plus"},
			want: "business",
		},
		{
			name:   "id token fallback",
			claims: map[string]any{},
			blob:   map[string]any{"id_token": idBusiness, "plan_type": "plus"},
			want:   "business",
		},
		{
			name:   "blob fallback",
			claims: map[string]any{},
			blob:   map[string]any{"plan_type": "BUSINESS"},
			want:   "business",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialPlanType(tc.claims, tc.blob); got != tc.want {
				t.Fatalf("credentialPlanType = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTeam332ActivityLabelsTicketAndExplainsMissingResponse(t *testing.T) {
	resetTicketActivitiesForTest(t)
	cfg := teamDemandSetup(t, func(http.ResponseWriter, *http.Request) { t.Fatal("cached Team ticket triggered a probe") })
	issued := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	rec := storeRecord{
		AuthID: probeTestAccount, Model: "model-a", Len: 332,
		Value: fakeTeamToken(issued, 9), IssuedAt: issued.Format(time.RFC3339), HarvestedAt: issued.Format(time.RFC3339), PlanType: "team",
	}
	if err := writeStoreRecord(cfg.StoreDir, rec, 292); err != nil {
		t.Fatal(err)
	}
	req := demandRequest(rec.AuthID, rec.Model)
	req.RequestID = "team-332-no-response-ticket"
	if resp := interceptAfter(t, req); resp.Terminate || outgoingHeader(resp) != rec.Value {
		t.Fatal("cached Team ticket was not injected")
	}
	raw, err := json.Marshal(pluginapi.ResponseInterceptRequest{
		RequestID: req.RequestID, Model: rec.Model, ResponseHeaders: http.Header{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handleMethod(pluginabi.MethodResponseInterceptAfter, raw); err != nil {
		t.Fatal(err)
	}
	activity := ticketActivitySnapshot()
	wantMissing := "上游未返回票（状态无法确认；下次仍注入请求332）"
	if len(activity) != 1 || activity[0].RequestTicket != "332" || activity[0].ResponseTicket != wantMissing {
		t.Fatalf("Team ticket activity labels are inaccurate: %+v", activity)
	}
}
