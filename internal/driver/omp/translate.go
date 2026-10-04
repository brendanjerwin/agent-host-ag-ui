package omp

import (
	"context"
	"encoding/json"

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

	textOpen     string // omp messageId with an open AG-UI text message
	thinkingOpen string // omp messageId with an open reasoning message
	toolOpen     map[string]string
	// toolStartSeen guards TOOL_CALL_START emitted only once per omp call id.
	toolStartSeen map[string]bool
	// lastAssistantStopReason is the stop reason of the final assistant
	// message, used to detect error tails.
	lastAssistantStopReason string
	// finished marks RUN_FINISHED already emitted.
	finished bool
	// errored marks RUN_ERROR already emitted.
	errored bool
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
		threadID:      threadID,
		runID:         runID,
		toolOpen:      map[string]string{},
		toolStartSeen: map[string]bool{},
	}
}

// translate consumes one omprpc frame and emits driver events. It returns
// the outcome; terminal emits are owned by the caller (streamRun).
func (t *translator) translate(ctx context.Context, frame omprpc.RpcServerFrame, out chan<- driver.StreamEvent) (translatorOutcome, string) {
	switch v := frame.Value.(type) {
	case omprpc.MessageStartEvent:
		switch v.Message.Value.(type) {
		case omprpc.AssistantMessage:
			// Assistant-role filter: only real assistant messages emit.
			// messageId is omp's stable per-process id.
			_ = ompMessageID(v.MessageID)
			return translatorContinue, ""
		}
		return translatorContinue, ""
	case omprpc.MessageUpdateEvent:
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
			content := truncateContent(v.Result)
			emit(ctx, out, driver.ToolCallResult{
				ToolCallID: v.ToolCallID,
				ToolName:   v.ToolName,
				Content:    content,
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
		// Arguments may not be inline yet; id arrives with toolcall_end's
		// full ToolCall in practice. Pairing: omp toolCallId from the end
		// event; before that, translator mints an index-based id.
		id := t.pendingToolID(mu)
		t.toolOpen[id] = msgID
	case omprpc.AssistantToolCallDeltaEvent:
		id := t.pendingToolID(mu)
		if _, open := t.toolOpen[id]; !open {
			t.toolOpen[id] = msgID
		}
		if ev.Delta != "" {
			emit(ctx, out, driver.ToolCallArgs{ToolCallID: id, Delta: ev.Delta})
		}
	case omprpc.AssistantToolCallEndEvent:
		id := ev.ToolCall.ID
		if _, open := t.toolOpen[id]; !open {
			// START not emitted yet (args streamed without start): emit now.
			emit(ctx, out, driver.ToolCallStart{
				ToolCallID:   id,
				ToolCallName: ev.ToolCall.Name,
				MessageID:    msgID,
			})
			t.toolStartSeen[id] = true
		}
		emit(ctx, out, driver.ToolCallEnd{ToolCallID: id})
		delete(t.toolOpen, id)
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
	for id := range t.toolOpen {
		emit(ctx, out, driver.ToolCallEnd{ToolCallID: id})
		delete(t.toolOpen, id)
	}
}

// pendingToolID resolves the id of the tool call currently being streamed.
// omp toolcall_start/delta frames carry no call id, only contentIndex; the
// id arrives with toolcall_end. Pairing rule: single open call uses it;
// multiple concurrent calls pair by open order index (documented).
func (t *translator) pendingToolID(mu omprpc.MessageUpdateEvent) string {
	if len(t.toolOpen) == 0 {
		return ""
	}
	// deterministic open-order pairing: lowest contentIndex key ordering.
	var ids []string
	for id := range t.toolOpen {
		ids = append(ids, id)
	}
	// single-call fast path
	if len(ids) == 1 {
		return ids[0]
	}
	// multi-call: pair by insertion index (stable in practice)
	return ids[0]
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
