package omp

import (
	"context"
	"testing"

	"github.com/brendanjerwin/agent-host-ag-ui/internal/driver"
	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
)

// fixture goldens decode omp stdout frames from docs/rpc.md §"Compact Command
// Flows" (1) Prompt and stream, verbatim, and assert the driver event
// sequence the translator emits.

// decodeOne decodes one omp stdout JSON line into an RpcServerFrame.
func decodeOne(t *testing.T, line string) omprpc.RpcServerFrame {
	t.Helper()
	var f omprpc.RpcServerFrame
	if err := f.UnmarshalJSON([]byte(line)); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return f
}

type drainFn func(ev driver.StreamEvent) bool

// collect runs translate over frames collecting events until the last
// (TerminalEvent). Returns the events emitted (excluding Terminal).
func collect(t *testing.T, frames []string) []driver.StreamEvent {
	t.Helper()
	out := make(chan driver.StreamEvent, 256)
	ctx := context.Background()
	tr := newTranslator("t1", "r1")
	for _, line := range frames {
		tr.translate(ctx, decodeOne(t, line), out)
	}
	// No terminal emitted by translate() itself; close and drain.
	close(out)
	var evs []driver.StreamEvent
	for ev := range out {
		evs = append(evs, ev)
	}
	return evs
}

// TestGoldenPromptAndStream locks the docs/rpc.md flow-1 sequence.
func TestGoldenPromptAndStream(t *testing.T) {
	frames := []string{
		`{ "id": "req_1", "type": "response", "command": "prompt", "success": true }`,
		`{ "type": "agent_start" }`,
		`{ "type": "message_update", "messageId": "msg-2", "assistantMessageEvent": { "type": "text_delta", "delta": "Hello world" }, "message": { "role": "assistant", "content": [] } }`,
		`{ "type": "agent_end", "messages": [], "isTerminal": true, "yielded": true }`,
		`{ "type": "prompt_result", "id": "req_1", "agentInvoked": true, "status": "completed", "sessionSettled": true }`,
		`{ "type": "session_settled" }`,
	}
	evs := collect(t, frames)
	want := []driver.StreamEvent{
		driver.TextDelta{MessageID: "msg-2", Text: "Hello world"},
		driver.RunFinished{ThreadID: "t1", RunID: "r1"},
	}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d: %#v", len(evs), len(want), evs)
	}
	for i, w := range want {
		if evs[i] != w {
			t.Errorf("event %d: got %#v, want %#v", i, evs[i], w)
		}
	}
}

// TestTextLifecycle locks START/CONTENT/END for one visible message.
func TestTextLifecycle(t *testing.T) {
	frames := []string{
		`{"type":"message_start","messageId":"m1","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"text_start","contentIndex":0,"partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"abc","partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"text_end","contentIndex":0,"content":"abc","partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_end","messageId":"m1","message":{"role":"assistant","content":[{"type":"text","text":"abc"}],"stopReason":"stop"}}`,
		`{"type":"agent_end","messages":[],"isTerminal":true,"yielded":true}`,
	}
	evs := collect(t, frames)
	var got []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case driver.TextDelta:
			got = append(got, "delta:"+e.Text+"@"+e.MessageID)
		case driver.RunFinished:
			got = append(got, "FINISHED")
		default:
			t.Errorf("unexpected event %#v", ev)
		}
	}
	if len(got) != 2 || got[0] != "delta:abc@m1" || got[1] != "FINISHED" {
		t.Fatalf("sequence: %#v", got)
	}
}

// TestThinkingLifecycle locks REASONING-style blocks mapping to ThinkingDelta.
func TestThinkingLifecycle(t *testing.T) {
	frames := []string{
		`{"type":"message_start","messageId":"m1","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"thinking_start","contentIndex":0,"partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"hmm","partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"thinking_end","contentIndex":0,"content":"hmm","partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"text_start","contentIndex":1,"partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"text_delta","contentIndex":1,"delta":"answer","partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"agent_end","messages":[],"isTerminal":true,"yielded":true}`,
	}
	evs := collect(t, frames)
	var got []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case driver.ThinkingDelta:
			got = append(got, "think:"+e.Text+"@"+e.MessageID)
		case driver.TextDelta:
			got = append(got, "text:"+e.Text+"@"+e.MessageID)
		case driver.RunFinished:
			got = append(got, "FINISHED")
		default:
			t.Errorf("unexpected event %#v", ev)
		}
	}
	want := []string{"think:hmm@m1", "text:answer@m1", "FINISHED"}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %s != %s", i, got[i], want[i])
		}
	}
}

// TestToolCallLifecycle locks the tool call open/args/close and result flow.
func TestToolCallLifecycle(t *testing.T) {
	frames := []string{
		`{"type":"message_start","messageId":"m1","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"toolcall_start","contentIndex":0,"partial":{"role":"assistant","content":[{"type":"toolCall","id":"toolu_1","name":"read","arguments":{"a":1}}]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"toolcall_delta","contentIndex":0,"delta":"{\"a\":","partial":{"role":"assistant","content":[{"type":"toolCall","id":"toolu_1","name":"read","arguments":{"a":1}}]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"toolcall_delta","contentIndex":0,"delta":"1}","partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"toolcall_end","contentIndex":0,"toolCall":{"type":"toolCall","id":"toolu_1","name":"read","arguments":{"a":1}},"partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"tool_execution_start","toolCallId":"toolu_1","toolName":"read","args":{"a":1}}`,
		`{"type":"tool_execution_end","toolCallId":"toolu_1","toolName":"read","result":"file body","isError":false}`,
		`{"type":"agent_end","messages":[],"isTerminal":true,"yielded":true}`,
	}
	evs := collect(t, frames)
	var kinds []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case driver.ToolCallStart:
			kinds = append(kinds, "start:"+e.ToolCallID+":"+e.ToolCallName)
		case driver.ToolCallArgs:
			kinds = append(kinds, "args:"+e.ToolCallID+":"+e.Delta)
		case driver.ToolCallEnd:
			kinds = append(kinds, "end:"+e.ToolCallID)
		case driver.ToolCallResult:
			kinds = append(kinds, "result:"+e.ToolCallID+":"+e.Content)
		case driver.RunFinished:
			kinds = append(kinds, "FINISHED")
		default:
			t.Errorf("unexpected event %#v", ev)
		}
	}
	want := []string{
		"start:tc-idx-0:read",
		"args:tc-idx-0:{\"a\":",
		"args:tc-idx-0:1}",
		"end:tc-idx-0",
		"result:tc-idx-0:file body",
		"FINISHED",
	}
	if len(kinds) != len(want) {
		t.Fatalf("got %#v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("event %d: %s != %s", i, kinds[i], want[i])
		}
	}
}

// TestErrorTerminal locks RUN_ERROR via prompt_result status error.
func TestErrorTerminal(t *testing.T) {
	frames := []string{
		`{"type":"agent_start"}`,
		`{"type":"prompt_result","id":"req_1","agentInvoked":true,"status":"error","sessionSettled":true,"error":{"message":"boom","retryable":false}}`,
	}
	evs := collect(t, frames)
	if len(evs) != 1 {
		t.Fatalf("got %#v", evs)
	}
	re, ok := evs[0].(driver.RunError)
	if !ok || re.Message != "boom" {
		t.Fatalf("got %#v", evs[0])
	}
}

// TestScreenshotRidesActivitySnapshot locks: browser_screenshot image
// results map to an ActivitySnapshot driver event (mime + base64), and
// unrelated text results keep going through ToolCallResult.
func TestScreenshotRidesActivitySnapshot(t *testing.T) {
	pngB64 := "iVBORw0KGgoA" // header bytes of a PNG, base64
	frames := []string{
		`{"type":"message_start","messageId":"m1","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"toolcall_start","contentIndex":0,"partial":{"role":"assistant","content":[{"type":"toolCall","id":"toolu_shot","name":"browser_screenshot","arguments":{}}]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","messageId":"m1","assistantMessageEvent":{"type":"toolcall_end","contentIndex":0,"toolCall":{"type":"toolCall","id":"toolu_shot","name":"browser_screenshot","arguments":{}},"partial":{"role":"assistant","content":[]}},"message":{"role":"assistant","content":[]}}`,
		`{"type":"tool_execution_end","toolCallId":"toolu_shot","toolName":"browser_screenshot","result":{"content":[{"type":"image","data":"` + pngB64 + `","mimeType":"image/png"}]},"isError":false}`,
		`{"type":"agent_end","messages":[],"isTerminal":true,"yielded":true}`,
	}
	evs := collect(t, frames)
	var kinds []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case driver.ActivitySnapshot:
			kinds = append(kinds, "activity:"+e.MimeType+":"+e.Base64)
		case driver.ToolCallStart:
			kinds = append(kinds, "start:"+e.ToolCallName)
		case driver.ToolCallEnd:
			kinds = append(kinds, "end")
		case driver.ToolCallResult:
			t.Errorf("screenshot fell through to text result: %#v", e)
		case driver.RunFinished:
			kinds = append(kinds, "FINISHED")
		default:
			t.Errorf("unexpected event %#v", ev)
		}
	}
	want := []string{"start:browser_screenshot", "end", "activity:image/png:" + pngB64, "FINISHED"}
	if len(kinds) != len(want) {
		t.Fatalf("got %#v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("event %d: %s != %s", i, kinds[i], want[i])
		}
	}
}

func TestAbortedNoEmission(t *testing.T) {
	frames := []string{
		`{"type":"agent_start"}`,
		`{"type":"prompt_result","id":"req_1","agentInvoked":true,"status":"aborted","sessionSettled":false}`,
	}
	evs := collect(t, frames)
	if len(evs) != 0 {
		t.Fatalf("got %#v", evs)
	}
}

// TestNonAssistantFilter locks: non-assistant records emit nothing.
func TestNonAssistantFilter(t *testing.T) {
	frames := []string{
		`{"type":"message_start","messageId":"m-aside","message":{"role":"custom","content":"aside"}}`,
		`{"type":"message_update","messageId":"m-aside","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"aside text","partial":{"role":"custom","content":[]}},"message":{"role":"custom","content":[]}}`,
		`{"type":"message_end","messageId":"m-aside","message":{"role":"custom","content":[]}}`,
	}
	evs := collect(t, frames)
	for _, ev := range evs {
		if _, ok := ev.(driver.TextDelta); ok {
			t.Fatalf("aside leaked: %#v", ev)
		}
	}
}
