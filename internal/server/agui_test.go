package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/brendanjerwin/agent-host-ag-ui/internal/driver"
)

// fakeDriver replays a canned run.
type fakeDriver struct {
	events []driver.StreamEvent
}

func (f *fakeDriver) Stream(ctx context.Context, req driver.RunRequest) (<-chan driver.StreamEvent, error) {
	out := make(chan driver.StreamEvent, len(f.events)+1)
	for _, ev := range f.events {
		out <- ev
	}
	out <- driver.TerminalEvent{Terminal: driver.TerminalFinished}
	close(out)
	return out, nil
}
func (f *fakeDriver) Cancel(threadID, runID string) error { return nil }
func (f *fakeDriver) Close() error                        { return nil }

// TestDecoderConformance pushes the adapter SSE through the AG-UI SDK
// decoder: zero unknown-family entries, invariants hold.
func TestDecoderConformance(t *testing.T) {
	fd := &fakeDriver{
		events: []driver.StreamEvent{
			driver.RunStarted{ThreadID: "smoke-1", RunID: "r1"},
			driver.TextDelta{MessageID: "m1", Text: "hello"},
			driver.TextDelta{MessageID: "m1", Text: " world"},
			driver.ThinkingDelta{MessageID: "m1", Text: "hmm"},
			driver.ToolCallStart{ToolCallID: "tc-idx-0", ToolCallName: "browser_snapshot", MessageID: "m1"},
			driver.ToolCallArgs{ToolCallID: "tc-idx-0", Delta: "{}"},
			driver.ToolCallEnd{ToolCallID: "tc-idx-0"},
			driver.ToolCallResult{ToolCallID: "tc-idx-0", ToolName: "browser_snapshot", Content: "page"},
			driver.RunFinished{ThreadID: "smoke-1", RunID: "r1"},
		},
	}
	srv := httptest.NewServer((&Server{Driver: fd}).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/ag-ui", strings.NewReader(`{
		"threadId": "smoke-1", "runId": "r1",
		"messages": [{"id":"m1","role":"user","content":"Reply with exactly: hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// Parse SSE frames.
	var sseEvents []string
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			sseEvents = append(sseEvents, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(sseEvents) == 0 {
		t.Fatalf("no SSE events in:\n%s", buf.String())
	}

	decoder := events.NewEventDecoder(nil)
	var sawStarted, sawFinished bool
	var startedThreadID, finishedThreadID string
	for i, data := range sseEvents {
		// Every AG-UI event JSON carries "type"; extract it for the decoder.
		var raw map[string]any
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			t.Fatalf("event %d not JSON: %v (%s)", i, err, data)
		}
		evType, _ := raw["type"].(string)
		dec, err := decoder.DecodeEvent(evType, []byte(data))
		if err != nil {
			t.Fatalf("event %d decode: %v (%s)", i, err, data)
		}
		switch e := dec.(type) {
		case *events.RunStartedEvent:
			sawStarted = true
			startedThreadID = e.ThreadIDValue
		case *events.RunFinishedEvent:
			sawFinished = true
			finishedThreadID = e.ThreadIDValue
		}
	}
	if !sawStarted {
		t.Error("missing RUN_STARTED")
	}
	if !sawFinished {
		t.Error("missing RUN_FINISHED")
	}
	if startedThreadID != "smoke-1" || finishedThreadID != "smoke-1" {
		t.Errorf("thread ids: %q / %q", startedThreadID, finishedThreadID)
	}
}

// TestInFlight409 locks the second-run conflict contract.
func TestInFlight409(t *testing.T) {
	// driver that never finishes: run blocks on channel
	blocked := make(chan struct{})
	fd := &blockingDriver{block: blocked}
	srv := httptest.NewServer((&Server{Driver: fd}).Handler())
	defer srv.Close()

	do := func() *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/ag-ui", strings.NewReader(`{
			"threadId": "t", "runId": "r1",
			"messages": [{"id":"m1","role":"user","content":"hi"}]
		}`))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp1 := do()
	defer resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("first run status %d", resp1.StatusCode)
	}
	resp2 := do()
	defer resp2.Body.Close()
	if resp2.StatusCode != 409 {
		t.Fatalf("second run status %d, want 409", resp2.StatusCode)
	}
	close(blocked)
}

// TestValidation400 locks the strict input contract.
func TestValidation400(t *testing.T) {
	cases := []string{
		`{"messages":[]}`,
		`{"threadId":"t","messages":[]}`,
		`{"threadId":"t","messages":[{"id":"m","role":"assistant","content":"hi"}]}`,
		`{bad json}`,
	}
	for _, body := range cases {
		fd := &fakeDriver{}
		srv := httptest.NewServer((&Server{Driver: fd}).Handler())
		req, _ := http.NewRequest("POST", srv.URL+"/ag-ui", strings.NewReader(body))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("body %q: status %d, want 400", body, resp.StatusCode)
		}
		srv.Close()
	}
}

// blockingDriver blocks the run until block closes; enforces one run per
// thread like the real omp driver.
type blockingDriver struct {
	block chan struct{}
	mu    sync.Mutex
	inRun bool
}

func (b *blockingDriver) Stream(ctx context.Context, req driver.RunRequest) (<-chan driver.StreamEvent, error) {
	b.mu.Lock()
	if b.inRun {
		b.mu.Unlock()
		return nil, driver.ErrRunInFlight
	}
	b.inRun = true
	b.mu.Unlock()
	out := make(chan driver.StreamEvent, 2)
	out <- driver.RunStarted{ThreadID: req.ThreadID, RunID: req.RunID}
	go func() {
		defer func() {
			b.mu.Lock()
			b.inRun = false
			b.mu.Unlock()
		}()
		select {
		case <-b.block:
		case <-ctx.Done():
		}
		close(out)
	}()
	return out, nil
}
func (b *blockingDriver) Cancel(threadID, runID string) error { return nil }
func (b *blockingDriver) Close() error                        { return nil }
