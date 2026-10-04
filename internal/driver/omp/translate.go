package omp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/brendanjerwin/agent-host-ag-ui/internal/driver"
	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
)

// translator state drives omp frames -> driver.StreamEvents for one run.
// Invariants enforced (table-driven tests lock these):
//   - first emission is RunStarted (owned by streamRun, not here);
//   - exactly one terminal outcome (FINISHED or ERROR), never both;
//   - messageId blocks open/close at most once;
//   - TOOL_CALL_ARGS only under an open START;
//   - assistant-role filtering: injected advisor/aside records (distinct
//     messageIds) never emit.
type translator struct {
	threadID string
	runID    string

	textOpen     string // omp messageId with an open text block
	thinkingOpen string // omp messageId with an open thinking block
	// toolByIndex maps omp contentIndex -> tool call state for calls whose
	// id arrives only at toolcall_end. An open state holds a placeholder
	// id (idx-<n>) adopted/renamed when the end frame arrives.
	toolByIndex map[int64]*openTool
	// ompToAGUI maps omp's real toolCallId (from toolcall_end and
	// tool_execution_end) to the stable AG-UI-facing placeholder id.
	ompToAGUI map[string]string
	// toolNames caches omp toolCallId -> display name for results.
	toolNames map[string]string
	// lastAssistantStopReason is the stop reason of the final assistant
	// message, used to detect error tails.
	lastAssistantStopReason string
	// finished marks RUN_FINISHED already emitted.
	finished bool
	// errored marks RUN_ERROR already emitted.
	errored bool
}

// openTool tracks one streaming tool call keyed by omp contentIndex.
type openTool struct {
	// id is the AG-UI-facing id: the real omp id once known, else a
	// placeholder that is stable within the run.
	id   string
	name string
	// started marks a ToolCallStart already emitted for this call.
	started bool
}

// translatorOutcome describes how translate() concluded for a frame.
type translatorOutcome int

const (
	// translatorContinue: keep consuming frames.
	translatorContinue translatorOutcome = iota
	// translatorDone: prompt_result arrived; status is its omp status.
	translatorDone
	// translatorAborted: prompt_result status aborted; no wire emission owed.
	translatorAborted
)

func newTranslator(threadID, runID string) *translator {
	return &translator{
		threadID:    threadID,
		runID:       runID,
		toolByIndex: map[int64]*openTool{},
		ompToAGUI:   map[string]string{},
		toolNames:   map[string]string{},
	}
}

// translate consumes one omprpc frame and emits driver events. It returns
// the outcome; terminal emits are owned by the caller (streamRun).
func (t *translator) translate(ctx context.Context, frame omprpc.RpcServerFrame, out chan<- driver.StreamEvent) (translatorOutcome, string) {
	switch v := frame.Value.(type) {
	case omprpc.MessageStartEvent:
		// Assistant-role filter: only real assistant messages emit.
		return translatorContinue, ""
	case omprpc.MessageUpdateEvent:
		if !messageIsAssistant(v.Message) {
			return translatorContinue, ""
		}
		t.translateAssistantEvent(ctx, v, out)
		return translatorContinue, ""
	case omprpc.MessageEndEvent:
		if m, ok := v.Message.Value.(omprpc.AssistantMessage); ok {
			t.lastAssistantStopReason = string(m.StopReason)
			// Close any still-open blocks defensively, exactly once.
			t.closeOpenBlocks(ctx, out)
		}
		return translatorContinue, ""
	case omprpc.ToolExecutionEndEvent:
		if len(v.Result) > 0 {
			// Resolve to the stable AG-UI-facing id; unmatched omp ids
			// (subagent/detail calls) mint their own.
			aguiID, ok := t.ompToAGUI[v.ToolCallID]
			if !ok {
				aguiID = "tc-exec-" + v.ToolCallID
			}
			content := truncateContent(v.Result)
			emit(ctx, out, driver.ToolCallResult{
				ToolCallID: aguiID,
				ToolName:   v.ToolName,
				Content:    content,
				IsError:    v.IsError != nil && *v.IsError,
			})
		}
		return translatorContinue, ""
	case omprpc.AgentEndEvent:
		// Terminal when isTerminal is nil or true; yielded means the turn
		// ended and resumes only for queued input. Missing-yield frames
		// default terminal (verified fallback rule).
		terminal := v.IsTerminal == nil || *v.IsTerminal
		yielded := v.Yielded == nil || *v.Yielded
		if terminal && yielded {
			t.closeOpenBlocks(ctx, out)
			if !t.finished && !t.errored {
				emit(ctx, out, driver.RunFinished{ThreadID: t.threadID, RunID: t.runID})
				t.finished = true
			}
		}
		return translatorContinue, ""
	case omprpc.PromptResultEvent:
		switch v.Status {
		case omprpc.PromptStatusCompleted:
			if !t.finished && !t.errored {
				t.closeOpenBlocks(ctx, out)
				emit(ctx, out, driver.RunFinished{ThreadID: t.threadID, RunID: t.runID})
				t.finished = true
			}
			return translatorDone, "completed"
		case omprpc.PromptStatusError:
			if !t.finished && !t.errored {
				t.closeOpenBlocks(ctx, out)
				msg := "omp run failed"
				if v.Error != nil {
					msg = v.Error.Message
				}
				emit(ctx, out, driver.RunError{ThreadID: t.threadID, RunID: t.runID, Message: msg})
				t.errored = true
			}
			return translatorDone, "error"
		case omprpc.PromptStatusAborted:
			return translatorAborted, "aborted"
		}
		return translatorDone, string(v.Status)
	case omprpc.SessionSettledEvent:
		// Post-terminal quiet marker; nothing to emit.
		return translatorContinue, ""
	case omprpc.UnknownNotification:
		return translatorContinue, ""
	default:
		return translatorContinue, ""
	}
}

// messageIsAssistant reports whether the AgentMessage is an assistant one.
func messageIsAssistant(m omprpc.AgentMessage) bool {
	_, ok := m.Value.(omprpc.AssistantMessage)
	return ok
}

// translateAssistantEvent maps one assistantMessageEvent to driver events.
func (t *translator) translateAssistantEvent(ctx context.Context, mu omprpc.MessageUpdateEvent, out chan<- driver.StreamEvent) {
	msgID := ompMessageID(mu.MessageID)
	switch ev := mu.AssistantMessageEvent.Value.(type) {
	case omprpc.AssistantStartEvent:
		// A new assistant message: ensure prior open blocks are closed.
		t.closeOpenBlocks(ctx, out)
	case omprpc.AssistantTextStartEvent:
		t.closeOpenBlocks(ctx, out)
		t.textOpen = msgID
	case omprpc.AssistantTextDeltaEvent:
		if t.textOpen == "" {
			t.textOpen = msgID
		}
		if ev.Delta != "" {
			emit(ctx, out, driver.TextDelta{MessageID: t.textOpen, Text: ev.Delta})
		}
	case omprpc.AssistantTextEndEvent:
		if t.textOpen != "" {
			t.textOpen = ""
		}
	case omprpc.AssistantThinkingStartEvent:
		t.closeOpenBlocks(ctx, out)
		t.thinkingOpen = msgID
	case omprpc.AssistantThinkingDeltaEvent:
		if t.thinkingOpen == "" {
			t.thinkingOpen = msgID
		}
		if ev.Delta != "" {
			emit(ctx, out, driver.ThinkingDelta{MessageID: t.thinkingOpen, Text: ev.Delta})
		}
	case omprpc.AssistantThinkingEndEvent:
		if t.thinkingOpen != "" {
			t.thinkingOpen = ""
		}
	case omprpc.AssistantToolCallStartEvent:
		// The id arrives only at toolcall_end; mint a stable placeholder
		// keyed by contentIndex and adopt the real id on end.
		ot := &openTool{id: fmt.Sprintf("tc-idx-%d", ev.ContentIndex)}
		t.toolByIndex[ev.ContentIndex] = ot
		emit(ctx, out, driver.ToolCallStart{
			ToolCallID:   ot.id,
			ToolCallName: toolNameFromPartial(ev.Partial, ev.ContentIndex),
			MessageID:    msgID,
		})
		ot.started = true
	case omprpc.AssistantToolCallDeltaEvent:
		ot, ok := t.toolByIndex[ev.ContentIndex]
		if !ok {
			// delta without start: synthesize the start first.
			ot = &openTool{id: fmt.Sprintf("tc-idx-%d", ev.ContentIndex)}
			t.toolByIndex[ev.ContentIndex] = ot
			emit(ctx, out, driver.ToolCallStart{
				ToolCallID:   ot.id,
				ToolCallName: toolNameFromPartial(ev.Partial, ev.ContentIndex),
				MessageID:    msgID,
			})
			ot.started = true
		}
		if ev.Delta != "" {
			emit(ctx, out, driver.ToolCallArgs{ToolCallID: ot.id, Delta: ev.Delta})
		}
	case omprpc.AssistantToolCallEndEvent:
		ot, ok := t.toolByIndex[ev.ContentIndex]
		if !ok {
			// never streamed: mint an AG-UI id and register the omp id.
			ot = &openTool{id: fmt.Sprintf("tc-omp-%s", ev.ToolCall.ID), started: false}
		}
		ot.name = ev.ToolCall.Name
		// Register the AG-UI-facing id for later tool_execution_end pairing;
		// the AG-UI id stays stable across START/ARGS/RESULT/END.
		t.ompToAGUI[ev.ToolCall.ID] = ot.id
		if !ot.started {
			emit(ctx, out, driver.ToolCallStart{
				ToolCallID:   ot.id,
				ToolCallName: ev.ToolCall.Name,
				MessageID:    msgID,
			})
		}
		emit(ctx, out, driver.ToolCallEnd{ToolCallID: ot.id})
		delete(t.toolByIndex, ev.ContentIndex)
	case omprpc.AssistantErrorEvent:
		// Assistant-level error: handled via prompt_result; keep consuming.
	case omprpc.AssistantDoneEvent:
		// Turn-local done marker; terminal owned by agent_end/prompt_result.
	}
}

// closeOpenBlocks emits closing ends for any still-open text or thinking
// blocks, exactly once each, defensively.
func (t *translator) closeOpenBlocks(ctx context.Context, out chan<- driver.StreamEvent) {
	if t.textOpen != "" {
		t.textOpen = ""
	}
	if t.thinkingOpen != "" {
		t.thinkingOpen = ""
	}
	for idx, ot := range t.toolByIndex {
		if ot.started {
			emit(ctx, out, driver.ToolCallEnd{ToolCallID: ot.id})
		}
		delete(t.toolByIndex, idx)
	}
}

// toolNameFromPartial extracts the tool call name from the partial message
// content at contentIndex, if present.
func toolNameFromPartial(partial omprpc.AssistantMessage, idx int64) string {
	if int(idx) < len(partial.Content) {
		if tc, ok := partial.Content[idx].Value.(omprpc.ToolCall); ok {
			return tc.Name
		}
	}
	return ""
}

// ompMessageID extracts omp's stable messageId, or derives a fallback.
func ompMessageID(id *string) string {
	if id != nil {
		return *id
	}
	return ""
}

// truncateContent renders tool result content to a bounded string.
func truncateContent(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return truncateRunes(s, 32_000)
	}
	return truncateRunes(string(raw), 32_000)
}

// truncateRunes shortens a string to at most max runes.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…[truncated]"
}
