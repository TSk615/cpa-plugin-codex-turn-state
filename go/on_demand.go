package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The margin covers time between the final gate and CPA sending the HTTP request.
// It is not a claim that the upstream will accept a locally valid token.
const demandExpiryMargin = 5 * time.Second

var (
	errAccountReauthorizationRequired = errors.New("account requires reauthorization")
	errNoValidTicket                  = errors.New("no valid ticket acquired")
)

type probeAccountCooldownError struct {
	Status probeAccountBlockStatus
}

func (e *probeAccountCooldownError) Error() string {
	if e == nil {
		return "account cooldown"
	}
	return fmt.Sprintf("account cooldown: %s (%d seconds left)", e.Status.Reason, e.Status.SecondsLeft)
}

func validateDemandConfig(cfg pluginConfig) error {
	if cfg.isProbe() || cfg.DryRun {
		return fmt.Errorf("on_demand requires role=business and dry_run=false")
	}
	if cfg.StoreDir == "" || cfg.ProbeManagementKey == "" {
		return fmt.Errorf("on_demand requires store_dir and probe_management_key")
	}
	if cfg.TemplateLength != 292 || cfg.TTLSeconds <= 5 || cfg.TTLSeconds > 3600 {
		return fmt.Errorf("on_demand requires template_length=292 and ttl_seconds between 6 and 3600")
	}
	if len(cfg.OnDemandAccounts) == 0 {
		return fmt.Errorf("on_demand_accounts must explicitly select at least one credential filename")
	}
	for _, account := range cfg.OnDemandAccounts {
		if account != strings.TrimSpace(account) || !strings.HasPrefix(account, "codex-") || !strings.HasSuffix(account, ".json") {
			return fmt.Errorf("on_demand_accounts requires exact Codex credential filenames")
		}
		if _, err := bucketRelPath(account, "model"); err != nil {
			return fmt.Errorf("invalid on_demand account filename")
		}
	}
	for _, model := range cfg.OnDemandModels {
		if model == "" || model != strings.TrimSpace(model) || strings.ContainsAny(model, " \t\r\n") {
			return fmt.Errorf("on_demand_models requires exact model ids without whitespace")
		}
		if _, err := bucketRelPath("codex-account.json", model); err != nil {
			return fmt.Errorf("invalid on_demand model id")
		}
	}
	return nil
}

func demandSelected(cfg pluginConfig, account string) bool {
	for _, name := range cfg.OnDemandAccounts {
		if name == account {
			return true
		}
	}
	return false
}

// An empty model list preserves the pre-model-filter configuration: all models
// of a selected account are gated. New dashboard saves always send an explicit
// list, allowing internal aliases such as codex-auto-review to bypass the gate.
func demandModelSelected(cfg pluginConfig, model string) bool {
	if len(cfg.OnDemandModels) == 0 {
		return true
	}
	for _, name := range cfg.OnDemandModels {
		if name == model {
			return true
		}
	}
	return false
}

// Match CPA's thinking.ParseSuffix: reasoning suffixes are not model IDs.
// Never use RequestedModel (an alias or pool name) as the ticket's bucket.
func demandModel(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "("); i >= 0 && strings.HasSuffix(model, ")") {
		model = model[:i]
	}
	return model
}

func demandTicketValid(cfg pluginConfig, value string, now time.Time) bool {
	issued, ok := fernetIssuedAt(value)
	return isTemplateLength(len(value), 292) && ok && !issued.After(now) && now.Add(demandExpiryMargin).Before(issued.Add(cfg.ttl()))
}

func demandReject(status int, code, message string) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{
		"type": "turn_state_error", "code": code, "message": message,
	}})
	// Returning an RPC error would make CPA skip the interceptor (fail open).
	// Always return a successful envelope containing an explicit termination.
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Terminate: true, StatusCode: status,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: body,
	})
}

func interceptOnDemand(req pluginapi.RequestInterceptRequest, cfg pluginConfig) (out []byte, err error) {
	// A plugin panic would otherwise be swallowed by the host and bypass this gate.
	defer func() {
		if recover() != nil {
			out, err = demandReject(503, "turn_state_internal", "Ticket gate failed; business request was not sent.")
		}
	}()
	account := metadataString(req.Metadata, selectedAuthMetadataKey)
	if account == "" {
		if req.ToFormat != "codex" {
			return noop()
		}
		return demandReject(503, "turn_state_identity_missing", "CPA did not identify the selected credential; refusing to guess its ticket.")
	}
	// In this mode no legacy injection or passive harvesting touches other accounts.
	if !demandSelected(cfg, account) {
		return noop()
	}
	if err := validateDemandConfig(cfg); err != nil {
		return demandReject(503, "turn_state_config", err.Error())
	}
	model := demandModel(req.Model)
	if model == "" {
		return demandReject(503, "turn_state_model_missing", "CPA did not identify the upstream model.")
	}
	if !demandModelSelected(cfg, model) {
		return noop()
	}
	if _, err := bucketRelPath(account, model); err != nil {
		return demandReject(503, "turn_state_model_invalid", "Invalid upstream model identifier.")
	}
	// CPA reuses upstream WebSockets; new HTTP headers do not update an existing
	// connection. Do not pretend this hook can guarantee per-request tickets there.
	if strings.EqualFold(headerValue(req.Headers, "Upgrade"), "websocket") {
		return demandReject(400, "turn_state_http_required", "On-demand tickets require HTTP/SSE requests; use POST /v1/responses instead of WebSocket.")
	}
	timeout := time.Duration(cfg.OnDemandTimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > ticketAcquireMaxWait {
		timeout = ticketAcquireMaxWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Remember whether this request already had a reusable ticket so the activity
	// log can distinguish a cache hit from an acquisition that spent quota.
	hadTicket := demandCached(cfg, account, model) != ""
	value, stats, acquireErr := demandEnsure(ctx, cfg, account, model, false)
	outcomes := formatAttemptStats(stats)
	if acquireErr != nil {
		message := demandAcquireErrorMessage(acquireErr) + "；" + outcomes
		recordTicketActivity("request", "failed", account, model, message, "")
		if errors.Is(acquireErr, context.DeadlineExceeded) || ctx.Err() != nil {
			return demandReject(504, "turn_state_timeout", "Timed out waiting for a valid 292/332 ticket; "+outcomes+"; business request was not sent.")
		}
		if errors.Is(acquireErr, errAccountReauthorizationRequired) {
			return demandReject(503, "turn_state_reauthorization_required", "The selected account returned HTTP 401 and needs reauthorization; business request was not sent.")
		}
		var cooldownErr *probeAccountCooldownError
		if errors.As(acquireErr, &cooldownErr) {
			return demandReject(503, "turn_state_account_cooldown", fmt.Sprintf("The selected account is cooling down after an upstream refusal (%d seconds left); business request was not sent. An operator may use Manual Ticket > Force Ticket to retry once during cooldown.", cooldownErr.Status.SecondsLeft))
		}
		return demandReject(503, "turn_state_unavailable", "No valid 292/332 ticket for the selected account and model; "+outcomes+"; business request was not sent.")
	}
	state.mu.Lock()
	current := reflect.DeepEqual(cfg, state.config)
	state.mu.Unlock()
	if !current || !demandTicketValid(cfg, value, time.Now()) {
		recordTicketActivity("request", "failed", account, model, "票已过期或配置在等待期间发生变化", "")
		return demandReject(503, "turn_state_changed", "Ticket expired or configuration changed while waiting; retry the request.")
	}
	result, message := "acquired", fmt.Sprintf("缺票，成功取得并注入 %d 票；%s", len(value), outcomes)
	if hadTicket {
		result, message = "reused", fmt.Sprintf("沿用未过期的 %d 票并放行；%s", len(value), outcomes)
	}
	recordTicketActivityForRequest("request", result, account, model, message, value, req.RequestID, ticketLengthLabelFor(value, cfg.TemplateLength, cfg.ReplaceLength))
	logDecision("inject", account, model, len(headerValue(req.Headers, turnStateHeader)), "on-demand: valid account/model ticket")
	return okEnvelope(pluginapi.RequestInterceptResponse{
		ClearHeaders: []string{turnStateHeader}, Headers: http.Header{turnStateHeader: {value}},
	})
}

type demandFlight struct {
	done  chan struct{}
	value string
	stats probeAttemptStats
	err   error
}
type demandAccountGate struct {
	token chan struct{}
	refs  int
}

var demandWork = struct {
	sync.Mutex
	flights  map[string]*demandFlight
	accounts map[string]*demandAccountGate
	slots    chan struct{}
}{flights: make(map[string]*demandFlight), accounts: make(map[string]*demandAccountGate), slots: make(chan struct{}, probeMaxAccountsInFlight)}

var refresh312Work = struct {
	sync.Mutex
	next map[string]time.Time
}{next: make(map[string]time.Time)}

// scheduleRefreshAfter312 reacts only to an explicit upstream 312 after this
// plugin injected a 292 or 332. One account/model can schedule at most one refresh per
// configured cooldown window; a burst of business responses therefore creates
// one acquisition flight, not one flight per response.
func scheduleRefreshAfter312(account, model string) {
	account = strings.TrimSpace(account)
	model = demandModel(model)
	if account == "" || model == "" {
		return
	}
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()
	if !cfg.OnDemand || !cfg.RefreshOn312 || !demandSelected(cfg, account) || !demandModelSelected(cfg, model) {
		return
	}
	now := time.Now()
	key := bucketKey(account, model)
	refresh312Work.Lock()
	if until := refresh312Work.next[key]; until.After(now) {
		refresh312Work.Unlock()
		logDecision("refresh-skip", account, model, cfg.ReplaceLength,
			fmt.Sprintf("response 312 refresh cooldown, %d seconds left", int64(until.Sub(now).Seconds())+1))
		return
	}
	refresh312Work.next[key] = now.Add(time.Duration(cfg.RefreshOn312Cooldown) * time.Second)
	refresh312Work.Unlock()

	if err := invalidateDemandTicket(cfg, account, model); err != nil {
		recordTicketActivity("refresh", "failed", account, model, "响应返回 312，但旧票失效处理失败："+probeRedact(err.Error()), "")
		return
	}
	logDecision("refresh", account, model, cfg.ReplaceLength, "response 312 invalidated cached ticket; scheduling one acquisition")
	go func() {
		timeout := time.Duration(cfg.OnDemandTimeoutSeconds) * time.Second
		if timeout <= 0 || timeout > ticketAcquireMaxWait {
			timeout = ticketAcquireMaxWait
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		value, stats, err := demandEnsure(ctx, cfg, account, model, false)
		summary := formatAttemptStats(stats)
		if err != nil {
			recordTicketActivity("refresh", "failed", account, model,
				"业务响应返回 312，自动刷新失败："+demandAcquireErrorMessage(err)+"；"+summary, "")
			return
		}
		recordTicketActivity("refresh", "acquired", account, model,
			fmt.Sprintf("业务响应返回 312，已重新取得并保存 %d；%s", len(value), summary), value)
	}()
}

func invalidateDemandTicket(cfg pluginConfig, account, model string) error {
	rel, err := bucketRelPath(account, model)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !reflect.DeepEqual(cfg, state.config) {
		return fmt.Errorf("configuration changed before 312 refresh")
	}
	err = os.Remove(filepath.Join(cfg.StoreDir, rel))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	key := bucketKey(account, model)
	delete(state.buckets, key)
	delete(state.store, key)
	state.storeMod = time.Time{}
	state.storeChecked = time.Time{}
	if errIndex := writeStoreIndex(cfg.StoreDir, time.Now(), cfg.ttl(), cfg.TemplateLength); errIndex != nil {
		return fmt.Errorf("ticket removed but index rewrite failed: %w", errIndex)
	}
	return nil
}

func demandCached(cfg pluginConfig, account, model string) string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !reflect.DeepEqual(cfg, state.config) {
		return ""
	}
	now := time.Now()
	state.refreshStoreLocked(cfg, now)
	entry, found := state.freshestTemplateLocked(account, model, now, cfg.ttl())
	// Records written before plan-aware ticket lengths have no plan_type. Do
	// not guess: forcing one re-acquisition is safer than replaying a Plus 292
	// as Team, or a Team ticket as Plus. The new record persists the plan.
	if found && strings.TrimSpace(entry.planType) != "" &&
		isAccountTemplateLength(len(entry.value), cfg.TemplateLength, entry.planType) &&
		demandTicketValid(cfg, entry.value, now) {
		return entry.value
	}
	return ""
}

func demandEnsure(ctx context.Context, cfg pluginConfig, account, model string, force bool) (value string, stats probeAttemptStats, err error) {
	if value := demandCached(cfg, account, model); value != "" {
		return value, probeAttemptStats{}, nil
	}
	key := bucketKey(account, model)
	if force {
		// A forced operator action must not join a normal flight that was rejected
		// by the local cooldown before making an upstream request.
		key += "\x00force"
	}
	demandWork.Lock()
	if flight := demandWork.flights[key]; flight != nil {
		demandWork.Unlock()
		select {
		case <-ctx.Done():
			return "", probeAttemptStats{}, ctx.Err()
		case <-flight.done:
			return flight.value, flight.stats, flight.err
		}
	}
	flight := &demandFlight{done: make(chan struct{})}
	demandWork.flights[key] = flight
	demandWork.Unlock()
	// No background acquisition: the leader runs synchronously within its deadline.
	// Publish even on panic, so followers never hang on an abandoned flight.
	defer func() {
		if recover() != nil {
			flight.err = fmt.Errorf("ticket acquisition failed")
			value, stats, err = "", flight.stats, flight.err
		}
		demandWork.Lock()
		delete(demandWork.flights, key)
		close(flight.done)
		demandWork.Unlock()
	}()
	acquireCtx, attemptBudget := withProbeAttemptCounter(ctx, int32(cfg.OnDemandMaxAttempts))
	flight.value, flight.err = demandAcquire(acquireCtx, cfg, account, model, force)
	flight.stats = attemptBudget.stats()
	return flight.value, flight.stats, flight.err
}

// Serialize different models of the same account too. Waiting is cancellable and
// gates are removed after the last waiter; idle accounts retain no worker/timer.
func demandLockAccount(ctx context.Context, account string) (func(), error) {
	demandWork.Lock()
	gate := demandWork.accounts[account]
	if gate == nil {
		gate = &demandAccountGate{token: make(chan struct{}, 1)}
		demandWork.accounts[account] = gate
	}
	gate.refs++
	demandWork.Unlock()
	drop := func() {
		demandWork.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(demandWork.accounts, account)
		}
		demandWork.Unlock()
	}
	select {
	case gate.token <- struct{}{}:
		return func() { <-gate.token; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func demandAcquire(ctx context.Context, cfg pluginConfig, account, model string, force bool) (string, error) {
	release, err := demandLockAccount(ctx, account)
	if err != nil {
		return "", err
	}
	defer release()
	select {
	case demandWork.slots <- struct{}{}:
		defer func() { <-demandWork.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if value := demandCached(cfg, account, model); value != "" {
		return value, nil
	}
	if block, blocked := probeAccountBlock(account, time.Now()); blocked && !force {
		if block.Status == "reauthorization_required" {
			return "", errAccountReauthorizationRequired
		}
		return "", &probeAccountCooldownError{Status: block}
	}
	if force {
		ctx = withProbeForceAttempt(ctx)
	}
	client := newProbeClient(cfg)
	defer client.http.CloseIdleConnections()
	blob, err := client.downloadAuth(ctx, account)
	if err != nil {
		return "", err
	}
	if stringField(blob, "type") != "codex" || blob["disabled"] == true {
		return "", fmt.Errorf("not an enabled Codex credential")
	}
	cred, err := probeParseCredential(account, blob)
	if err != nil {
		return "", err
	}
	if !cred.expiresAt.IsZero() && !cred.expiresAt.After(time.Now()) {
		return "", fmt.Errorf("credential expired")
	}
	pool := newProbeClientPool()
	defer pool.closeIdle()
	probeHarvestBucket(ctx, cfg, pool, cred, model, cfg.ProbeProxies, cfg.ProbeProxiesRotating, 0)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if value := demandCached(cfg, account, model); value != "" {
		return value, nil
	}
	if block, blocked := probeAccountBlock(account, time.Now()); blocked {
		if block.Status == "reauthorization_required" {
			return "", errAccountReauthorizationRequired
		}
		return "", &probeAccountCooldownError{Status: block}
	}
	if probeAttemptLimitReached(ctx) {
		return "", errTicketAttemptLimit
	}
	return "", errNoValidTicket
}

func demandAcquireErrorMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "等待有效票（292/332）超时"
	}
	if errors.Is(err, errTicketAttemptLimit) {
		return "已达到本次打票次数上限，仍未取得有效票（292/332）"
	}
	if errors.Is(err, errAccountReauthorizationRequired) {
		return "账号返回 HTTP 401，需要重新授权"
	}
	if errors.Is(err, errNoValidTicket) {
		return "没有取得有效票（292/332）"
	}
	var cooldownErr *probeAccountCooldownError
	if errors.As(err, &cooldownErr) {
		return fmt.Sprintf("账号因 %s 处于冷却，剩余 %d 秒", cooldownErr.Status.Reason, cooldownErr.Status.SecondsLeft)
	}
	return probeRedact(err.Error())
}
