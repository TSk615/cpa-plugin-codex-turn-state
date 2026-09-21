package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Codex normally emits one JSON object per SSE frame. Keep only an incomplete
// trailing frame so a transport split cannot hide codex.response.metadata.
// The bound prevents malformed or non-SSE traffic from retaining an answer.
const (
	maxResponseTicketFrameBytes = 128 << 10
	responseTicketScanMaxAge    = 5 * time.Minute
)

type responseTicketScanEntry struct {
	buffer  []byte
	updated time.Time
}

var responseTicketStreamScans = struct {
	sync.Mutex
	entries map[string]responseTicketScanEntry
}{entries: make(map[string]responseTicketScanEntry)}

func resetResponseTicketStreamScan(requestID string) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	responseTicketStreamScans.Lock()
	delete(responseTicketStreamScans.entries, requestID)
	responseTicketStreamScans.Unlock()
}

func scanResponseTicketStream(requestID string, chunk []byte) (string, bool) {
	if strings.TrimSpace(requestID) == "" || len(chunk) == 0 {
		return "", false
	}
	now := time.Now()
	responseTicketStreamScans.Lock()
	defer responseTicketStreamScans.Unlock()

	for id, entry := range responseTicketStreamScans.entries {
		if now.Sub(entry.updated) > responseTicketScanMaxAge {
			delete(responseTicketStreamScans.entries, id)
		}
	}
	entry := responseTicketStreamScans.entries[requestID]
	entry.buffer = append(entry.buffer, chunk...)
	entry.updated = now
	if len(entry.buffer) > maxResponseTicketFrameBytes {
		delete(responseTicketStreamScans.entries, requestID)
		return "", false
	}

	for {
		frame, rest, complete := takeSSEFrame(entry.buffer)
		if !complete {
			// Some executor paths provide a complete raw JSON event without SSE
			// framing. Accept it as soon as it is valid JSON.
			if value, found, terminal, valid := inspectTurnStateFrame(entry.buffer); valid {
				delete(responseTicketStreamScans.entries, requestID)
				if found {
					return value, true
				}
				if terminal {
					return "", false
				}
				return "", false
			}
			responseTicketStreamScans.entries[requestID] = entry
			return "", false
		}

		entry.buffer = rest
		value, found, terminal, _ := inspectTurnStateFrame(frame)
		if found || terminal {
			delete(responseTicketStreamScans.entries, requestID)
			return value, found
		}
		if len(entry.buffer) == 0 {
			delete(responseTicketStreamScans.entries, requestID)
			return "", false
		}
	}
}

func takeSSEFrame(buf []byte) (frame, rest []byte, complete bool) {
	lf := bytes.Index(buf, []byte("\n\n"))
	crlf := bytes.Index(buf, []byte("\r\n\r\n"))
	index, width := lf, 2
	if crlf >= 0 && (index < 0 || crlf < index) {
		index, width = crlf, 4
	}
	if index < 0 {
		return nil, buf, false
	}
	return buf[:index], buf[index+width:], true
}

func inspectTurnStateFrame(frame []byte) (value string, found, terminal, valid bool) {
	payload := sseDataPayload(frame)
	if len(payload) == 0 {
		return "", false, false, false
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return "", false, true, true
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return "", false, false, false
	}
	eventType, _ := event["type"].(string)
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return "", false, true, true
	case "codex.response.metadata", "response.metadata":
		headers, _ := event["headers"].(map[string]any)
		if headers == nil {
			if metadata, ok := event["metadata"].(map[string]any); ok {
				headers, _ = metadata["headers"].(map[string]any)
			}
		}
		for name, raw := range headers {
			if !strings.EqualFold(name, turnStateHeader) {
				continue
			}
			switch typed := raw.(type) {
			case string:
				if typed != "" {
					return typed, true, false, true
				}
			case []any:
				for _, item := range typed {
					if text, ok := item.(string); ok && text != "" {
						return text, true, false, true
					}
				}
			}
		}
	}
	return "", false, false, true
}

func sseDataPayload(frame []byte) []byte {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 {
		return nil
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	var data [][]byte
	for _, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data = append(data, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
	}
	if len(data) > 0 {
		return bytes.Join(data, []byte("\n"))
	}
	return trimmed
}
