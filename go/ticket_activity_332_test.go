package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestProbeAttemptCounts332Separately(t *testing.T) {
	ctx, budget := withProbeAttemptCounter(context.Background(), 10)
	for _, length := range []int{292, 312, 332, 332, 400} {
		if !probeCountAttempt(ctx) {
			t.Fatal("attempt unexpectedly blocked")
		}
		probeRecordAttemptResponse(ctx, http.StatusOK, strings.Repeat("x", length))
	}
	stats := budget.stats()
	if stats.Attempts != 5 || stats.Returned292 != 1 || stats.Returned312 != 1 || stats.Returned332 != 2 || stats.OtherHTTP != 1 {
		t.Fatalf("unexpected counters: %+v", stats)
	}
	if got := formatAttemptStats(stats); got != "本次打票尝试 5 次，292×1，312×1，332×2，其他返回×1" {
		t.Fatalf("unexpected summary: %s", got)
	}
	encoded, err := json.Marshal(ticketAcquireResult(ticketActivity{}, false, stats))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response["returned_332"] != float64(2) {
		t.Fatalf("returned_332 not exposed: %s", encoded)
	}
}

func TestProbe332HTTPErrorKeepsHTTPClassification(t *testing.T) {
	ctx, budget := withProbeAttemptCounter(context.Background(), 1)
	probeCountAttempt(ctx)
	probeRecordAttemptResponse(ctx, http.StatusForbidden, strings.Repeat("x", 332))
	stats := budget.stats()
	if stats.Forbidden403 != 1 || stats.Returned332 != 0 {
		t.Fatalf("unexpected counters: %+v", stats)
	}
	if strings.Contains(formatAttemptStats(stats), "332×") {
		t.Fatal("zero 332 count should not alter existing summaries")
	}
}
