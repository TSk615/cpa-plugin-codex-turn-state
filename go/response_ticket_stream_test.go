package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetResponseTicketStreamScansForTest(t *testing.T) {
	t.Helper()
	responseTicketStreamScans.Lock()
	old := responseTicketStreamScans.entries
	responseTicketStreamScans.entries = make(map[string]responseTicketScanEntry)
	responseTicketStreamScans.Unlock()
	t.Cleanup(func() {
		responseTicketStreamScans.Lock()
		responseTicketStreamScans.entries = old
		responseTicketStreamScans.Unlock()
	})
}

func TestResponseTicketStreamFindsSplitMetadataEvent(t *testing.T) {
	resetResponseTicketStreamScansForTest(t)
	ticket := fakeToken(292, time.Now())
	event, err := json.Marshal(map[string]any{
		"type": "codex.response.metadata",
		"headers": map[string]any{
			"X-Codex-Turn-State": ticket,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := append([]byte("data: "), event...)
	frame = append(frame, '\n', '\n')
	cut := len(frame) / 2
	if value, found := scanResponseTicketStream("split", frame[:cut]); found || value != "" {
		t.Fatal("partial frame was treated as a ticket")
	}
	value, found := scanResponseTicketStream("split", frame[cut:])
	if !found || value != ticket {
		t.Fatalf("metadata ticket not recovered after split: found=%t len=%d", found, len(value))
	}
}

func TestBusinessStreamMetadataUpdatesMissingResponseTicket(t *testing.T) {
	resetTicketActivitiesForTest(t)
	resetResponseTicketStreamScansForTest(t)
	cfg := demandSetup(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a valid cached ticket should not trigger a probe")
	})
	rec := storeRecordFor(probeTestAccount, "model-a", time.Now().Add(-time.Minute), 292)
	mustWriteRecord(t, cfg.StoreDir, rec)
	req := demandRequest(probeTestAccount, "model-a")
	req.RequestID = "business-stream-metadata"
	if resp := interceptAfter(t, req); resp.Terminate || outgoingHeader(resp) != rec.Value {
		t.Fatal("cached ticket was not injected")
	}

	callStreamChunkForTest(t, pluginapi.StreamChunkInterceptRequest{
		RequestID:  req.RequestID,
		Model:      "model-a",
		ChunkIndex: pluginapi.StreamChunkHeaderInitIndex,
	})
	activity := ticketActivitySnapshot()
	if len(activity) != 1 || activity[0].ResponseSource != "" {
		t.Fatalf("headerless stream should remain unresolved before metadata: %+v", activity)
	}

	responseTicket := fakeToken(312, time.Now())
	event, err := json.Marshal(map[string]any{
		"type": "codex.response.metadata",
		"headers": map[string]any{
			"x-codex-turn-state": responseTicket,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	callStreamChunkForTest(t, pluginapi.StreamChunkInterceptRequest{
		RequestID:  req.RequestID,
		Model:      "model-a",
		ChunkIndex: 0,
		Body:       append(append([]byte("data: "), event...), '\n', '\n'),
	})
	activity = ticketActivitySnapshot()
	if len(activity) != 1 || activity[0].RequestTicket != "292" || activity[0].ResponseTicket != "312" || activity[0].ResponseSource != "SSE metadata" {
		t.Fatalf("SSE metadata did not update the business response ticket: %+v", activity)
	}
}

func TestResponseTicketStreamStopsAtCompletedEvent(t *testing.T) {
	resetResponseTicketStreamScansForTest(t)
	if value, found := scanResponseTicketStream("done", []byte("data: {\"type\":\"response.completed\"}\n\n")); found || value != "" {
		t.Fatal("terminal event produced a ticket")
	}
	responseTicketStreamScans.Lock()
	_, retained := responseTicketStreamScans.entries["done"]
	responseTicketStreamScans.Unlock()
	if retained {
		t.Fatal("terminal event left a stream scan buffer behind")
	}
}

func callStreamChunkForTest(t *testing.T, req pluginapi.StreamChunkInterceptRequest) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = interceptStreamChunk(raw); err != nil {
		t.Fatal(err)
	}
}
