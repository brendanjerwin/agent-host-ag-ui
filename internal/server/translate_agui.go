package server

import (
	agui "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/brendanjerwin/agent-host-ag-ui/internal/driver"
)

// aguiEventType names the SSE event type line for a driver event.
func aguiEventType(ev driver.StreamEvent) agui.EventType {
	switch ev.(type) {
	case driver.RunStarted:
		return agui.EventTypeRunStarted
	case driver.TextDelta:
		return agui.EventTypeTextMessageContent
	case driver.ThinkingDelta:
		return agui.EventTypeReasoningMessageContent
	case driver.ToolCallStart:
		return agui.EventTypeToolCallStart
	case driver.ToolCallArgs:
		return agui.EventTypeToolCallArgs
	case driver.ToolCallEnd:
		return agui.EventTypeToolCallEnd
	case driver.ToolCallResult:
		return agui.EventTypeToolCallResult
	case driver.ActivitySnapshot:
		return agui.EventTypeActivitySnapshot
	case driver.RunFinished:
		return agui.EventTypeRunFinished
	case driver.RunError:
		return agui.EventTypeRunError
	default:
		return agui.EventTypeCustom
	}
}

// streamState tracks open message blocks for one SSE response and emits the
// AG-UI message lifecycle (START before first delta of a message, END before
// the message changes or the run ends). The omp driver stream reaches the
// face as deltas only; the visible lifecycle lives here.
type streamState struct {
	threadID string
	runID    string

	textOpen     string // messageId with an open TEXT_MESSAGE
	thinkingOpen string // messageId with an open REASONING_MESSAGE
}

// NewStreamState tracks open message blocks for one SSE response.
func NewStreamState(threadID, runID string) *streamState {
	return &streamState{threadID: threadID, runID: runID}
}

// closeText closes the open text message, if any.
func (st *streamState) closeText() []agui.Event {
	if st.textOpen == "" {
		return nil
	}
	ev := agui.NewTextMessageEndEvent(st.textOpen)
	st.textOpen = ""
	return []agui.Event{ev}
}

// closeThinking closes the open reasoning message, if any.
func (st *streamState) closeThinking() []agui.Event {
	if st.thinkingOpen == "" {
		return nil
	}
	ev := agui.NewReasoningMessageEndEvent(st.thinkingOpen)
	st.thinkingOpen = ""
	return []agui.Event{ev}
}

// toAGUI maps one driver event to zero or more AG-UI events (message
// START/END are synthesized around the first/last deltas of each message).
func (st *streamState) toAGUI(ev driver.StreamEvent) []agui.Event {
	switch v := ev.(type) {
	case driver.RunStarted:
		return []agui.Event{agui.NewRunStartedEvent(st.threadID, st.runID)}
	case driver.TextDelta:
		var out []agui.Event
		if st.textOpen == "" {
			// switch streams: close an open reasoning block first.
			out = append(out, st.closeThinking()...)
			out = append(out, agui.NewTextMessageStartEvent(v.MessageID, agui.WithRole("assistant")))
			st.textOpen = v.MessageID
		} else if st.textOpen != v.MessageID {
			out = append(out, agui.NewTextMessageEndEvent(st.textOpen))
			out = append(out, agui.NewTextMessageStartEvent(v.MessageID, agui.WithRole("assistant")))
			st.textOpen = v.MessageID
		}
		out = append(out, agui.NewTextMessageContentEvent(v.MessageID, v.Text))
		return out
	case driver.ThinkingDelta:
		var out []agui.Event
		if st.thinkingOpen == "" {
			out = append(out, st.closeText()...)
			out = append(out, agui.NewReasoningMessageStartEvent(v.MessageID, "assistant"))
			st.thinkingOpen = v.MessageID
		} else if st.thinkingOpen != v.MessageID {
			out = append(out, agui.NewReasoningMessageEndEvent(st.thinkingOpen))
			out = append(out, agui.NewReasoningMessageStartEvent(v.MessageID, "assistant"))
			st.thinkingOpen = v.MessageID
		}
		out = append(out, agui.NewReasoningMessageContentEvent(v.MessageID, v.Text))
		return out
	case driver.ToolCallStart:
		return []agui.Event{agui.NewToolCallStartEvent(v.ToolCallID, v.ToolCallName)}
	case driver.ToolCallArgs:
		return []agui.Event{agui.NewToolCallArgsEvent(v.ToolCallID, v.Delta)}
	case driver.ToolCallEnd:
		return []agui.Event{agui.NewToolCallEndEvent(v.ToolCallID)}
	case driver.ToolCallResult:
		return []agui.Event{agui.NewToolCallResultEvent(v.ToolCallID, v.ToolCallID, v.Content)}
	case driver.ActivitySnapshot:
		return []agui.Event{agui.NewActivitySnapshotEvent(
			v.ToolCallID,
			"browser_screenshot",
			map[string]any{
				"screenshot": "data:" + v.MimeType + ";base64," + v.Base64,
				"mime":       v.MimeType,
			},
		)}
	case driver.RunFinished:
		out := append(st.closeThinking(), st.closeText()...)
		out = append(out, agui.NewRunFinishedEvent(st.threadID, st.runID))
		return out
	case driver.RunError:
		out := append(st.closeThinking(), st.closeText()...)
		out = append(out, agui.NewRunErrorEvent(v.Message))
		return out
	case driver.TerminalEvent:
		out := append(st.closeThinking(), st.closeText()...)
		return out
	default:
		return nil
	}
}
