package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ticketActivity is deliberately value-free: the dashboard can explain what
// happened without ever receiving the credential-adjacent ticket blob.
type ticketActivity struct {
	At             string `json:"at"`
	Kind           string `json:"kind"`   // manual, request or passive
	Result         string `json:"result"` // acquired, reused or failed
	Account        string `json:"account"`
	Model          string `json:"model"`
	RequestTicket  string `json:"request_ticket,omitempty"`
	ResponseTicket string `json:"response_ticket,omitempty"`
	Message        string `json:"message,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	SecondsLeft    int64  `json:"seconds_left,omitempty"`
	RequestID      string `json:"-"`
	AuthID         string `json:"-"`
}

var ticketActivities = struct {
	sync.Mutex
	items []ticketActivity
}{}

const ticketActivityLimit = 50

func recordTicketActivity(kind, result, account, model, message, value string) ticketActivity {
	return recordTicketActivityForRequest(kind, result, account, model, message, value, "", "")
}

func recordTicketActivityForRequest(kind, result, account, model, message, value, requestID, requestTicket string) ticketActivity {
	now := time.Now()
	event := ticketActivity{
		At:            now.UTC().Format(time.RFC3339),
		Kind:          kind,
		Result:        result,
		Account:       maskAuthLabel(account),
		Model:         model,
		Message:       message,
		RequestTicket: requestTicket,
		RequestID:     requestID,
		AuthID:        account,
	}
	if issued, ok := fernetIssuedAt(value); ok {
		expires := issued.Add(time.Duration(currentTTLSeconds()) * time.Second)
		event.ExpiresAt = expires.UTC().Format(time.RFC3339)
		if left := int64(expires.Sub(now) / time.Second); left > 0 {
			event.SecondsLeft = left
		}
	}
	ticketActivities.Lock()
	ticketActivities.items = append([]ticketActivity{event}, ticketActivities.items...)
	if len(ticketActivities.items) > ticketActivityLimit {
		ticketActivities.items = ticketActivities.items[:ticketActivityLimit]
	}
	ticketActivities.Unlock()
	return event
}

func ticketLengthLabelFor(value string, templateLength, replaceLength int) string {
	switch len(value) {
	case templateLength:
		return "292"
	case replaceLength:
		return "312"
	case 0:
		return "无票头"
	default:
		return fmt.Sprintf("其他长度(%d)", len(value))
	}
}

func observeTicketResponse(requestID string, headers http.Header, model string, templateLength, replaceLength int) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	value := headerValue(headers, turnStateHeader)
	responseTicket := ticketLengthLabelFor(value, templateLength, replaceLength)
	var account string
	var authID string
	var requestTicket string
	var activityModel string
	var matched bool
	ticketActivities.Lock()
	for i := range ticketActivities.items {
		if ticketActivities.items[i].RequestID != requestID {
			continue
		}
		// Codex turn-state is a per-turn sticky-routing token. After the first
		// response issues it, successful continuation responses commonly omit the
		// header; the client keeps replaying the existing value unchanged. Make
		// that distinct from a headerless request that never carried a ticket.
		if len(value) == 0 && ticketActivities.items[i].RequestTicket == "292" {
			responseTicket = "上游未返回票（状态无法确认；下次仍注入请求292）"
		}
		ticketActivities.items[i].ResponseTicket = responseTicket
		if strings.TrimSpace(model) != "" && ticketActivities.items[i].Model == "" {
			ticketActivities.items[i].Model = model
		}
		account = ticketActivities.items[i].Account
		authID = ticketActivities.items[i].AuthID
		requestTicket = ticketActivities.items[i].RequestTicket
		activityModel = ticketActivities.items[i].Model
		matched = true
		break
	}
	ticketActivities.Unlock()
	if matched {
		logDecision("response", account, model, len(value), "业务响应票="+responseTicket)
		if len(value) == replaceLength && requestTicket == "292" {
			if strings.TrimSpace(model) == "" {
				model = activityModel
			}
			scheduleRefreshAfter312(authID, model)
		}
	}
}

func currentTTLSeconds() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.config.TTLSeconds
}

func ticketActivitySnapshot() []ticketActivity {
	ticketActivities.Lock()
	defer ticketActivities.Unlock()
	out := append([]ticketActivity(nil), ticketActivities.items...)
	if out == nil {
		out = []ticketActivity{}
	}
	return out
}

type ticketAcquireResponse struct {
	Success           bool   `json:"success"`
	Result            string `json:"result"`
	Account           string `json:"account"`
	Model             string `json:"model"`
	ExpiresAt         string `json:"expires_at,omitempty"`
	SecondsLeft       int64  `json:"seconds_left,omitempty"`
	Message           string `json:"message"`
	Forced            bool   `json:"forced,omitempty"`
	Attempts          int32  `json:"attempts"`
	Returned292       int32  `json:"returned_292"`
	Returned312       int32  `json:"returned_312"`
	Unauthorized401   int32  `json:"unauthorized_401"`
	Forbidden403      int32  `json:"forbidden_403"`
	RateLimited429    int32  `json:"rate_limited_429"`
	OtherHTTP         int32  `json:"other_http"`
	MissingTurnState  int32  `json:"missing_turn_state"`
	TransportFailures int32  `json:"transport_failures"`
	OutcomeSummary    string `json:"outcome_summary"`
}

func formatAttemptStats(stats probeAttemptStats) string {
	if stats.Attempts == 0 {
		return "本次打票尝试 0 次（未请求上游）"
	}
	parts := []string{
		fmt.Sprintf("本次打票尝试 %d 次", stats.Attempts),
		fmt.Sprintf("292×%d", stats.Returned292),
		fmt.Sprintf("312×%d", stats.Returned312),
	}
	if stats.Unauthorized401 > 0 {
		parts = append(parts, fmt.Sprintf("401×%d", stats.Unauthorized401))
	}
	if stats.Forbidden403 > 0 {
		parts = append(parts, fmt.Sprintf("403×%d", stats.Forbidden403))
	}
	if stats.RateLimited429 > 0 {
		parts = append(parts, fmt.Sprintf("429×%d", stats.RateLimited429))
	}
	if stats.MissingTurnState > 0 {
		parts = append(parts, fmt.Sprintf("无票头×%d", stats.MissingTurnState))
	}
	if stats.OtherHTTP > 0 {
		parts = append(parts, fmt.Sprintf("其他返回×%d", stats.OtherHTTP))
	}
	if stats.TransportFailures > 0 {
		parts = append(parts, fmt.Sprintf("网络失败×%d", stats.TransportFailures))
	}
	return strings.Join(parts, "，")
}

func ticketAcquireResult(event ticketActivity, force bool, stats probeAttemptStats) ticketAcquireResponse {
	return ticketAcquireResponse{
		Success: true, Result: event.Result, Account: event.Account, Model: event.Model,
		ExpiresAt: event.ExpiresAt, SecondsLeft: event.SecondsLeft, Message: event.Message, Forced: force,
		Attempts: stats.Attempts, Returned292: stats.Returned292, Returned312: stats.Returned312,
		Unauthorized401: stats.Unauthorized401, Forbidden403: stats.Forbidden403,
		RateLimited429: stats.RateLimited429, OtherHTTP: stats.OtherHTTP,
		MissingTurnState: stats.MissingTurnState, TransportFailures: stats.TransportFailures,
		OutcomeSummary: formatAttemptStats(stats),
	}
}

// handleTicketAcquireResource performs one explicit account/model acquisition.
// It reuses a live ticket without spending a request, otherwise it waits for one
// upstream probe and reports the outcome. No ticket value crosses this route.
func handleTicketAcquireResource(q url.Values) pluginapi.ManagementResponse {
	account := strings.TrimSpace(q.Get("account"))
	model := demandModel(q.Get("model"))
	force := queryTrue(q.Get("force"))
	if account == "" || model == "" {
		return managementError(http.StatusBadRequest, "account and model are required")
	}
	if !strings.HasPrefix(account, "codex-") || !strings.HasSuffix(account, ".json") {
		return managementError(http.StatusBadRequest, "account must be an exact Codex credential filename")
	}
	if _, err := bucketRelPath(account, model); err != nil {
		return managementError(http.StatusBadRequest, "invalid account or model")
	}

	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()
	if strings.TrimSpace(cfg.StoreDir) == "" || strings.TrimSpace(cfg.ProbeManagementKey) == "" {
		return managementError(http.StatusConflict, "store_dir and probe_management_key are required")
	}
	if cfg.OnDemand && !demandSelected(cfg, account) {
		return managementError(http.StatusConflict, "this account is not selected in on-demand settings")
	}

	if value := demandCached(cfg, account, model); value != "" {
		event := recordTicketActivity("manual", "reused", account, model, "已有有效票，无需重复打票", value)
		return jsonResponse(http.StatusOK, ticketAcquireResult(event, force, probeAttemptStats{}))
	}

	timeout := time.Duration(cfg.OnDemandTimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > ticketAcquireMaxWait {
		timeout = ticketAcquireMaxWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	value, stats, err := demandEnsure(ctx, cfg, account, model, force)
	summary := formatAttemptStats(stats)
	if err != nil {
		message := demandAcquireErrorMessage(err) + "；" + summary
		recordTicketActivity("manual", "failed", account, model, message, "")
		if errors.Is(err, errTicketAttemptLimit) {
			return managementError(http.StatusServiceUnavailable, fmt.Sprintf("打票失败：已达到本次 %d 次上限；%s，未取得 292", cfg.OnDemandMaxAttempts, summary))
		}
		if errors.Is(err, errAccountReauthorizationRequired) {
			return managementError(http.StatusServiceUnavailable, fmt.Sprintf("打票失败：账号返回 HTTP 401，需要重新授权；%s。重新授权后可用“强制打票”立即验证", summary))
		}
		var cooldownErr *probeAccountCooldownError
		if errors.As(err, &cooldownErr) {
			return managementError(http.StatusServiceUnavailable, fmt.Sprintf("打票失败：账号因 %s 处于冷却，剩余 %d 秒；%s。可使用“强制打票”绕过本次本地冷却", cooldownErr.Status.Reason, cooldownErr.Status.SecondsLeft, summary))
		}
		return managementError(http.StatusServiceUnavailable, fmt.Sprintf("打票失败：%s", message))
	}
	event := recordTicketActivity("manual", "acquired", account, model, "成功取得并保存 292 票；"+summary, value)
	return jsonResponse(http.StatusOK, ticketAcquireResult(event, force, stats))
}
