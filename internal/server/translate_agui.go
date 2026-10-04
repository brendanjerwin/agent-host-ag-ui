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
	case driver.RunFinished:
		return agui.EventTypeRunFinished
	case driver.RunError:
		return agui.EventTypeRunError
	default:
		return agui.EventTypeCustom
	}
}

// toAGUI converts a driver event to an AG-UI SDK event carrying
// threadId/runId. The translator emits block lifecycle in order; visible
// text messages open with TEXT_MESSAGE_START before CONTENT and close with
// END (tracked here since the driver stream carries deltas only).
type streamState struct {
	threadID string
	runID    string
	// open message ids that need START before next CONTENT / END after.
	textStarted map[string]bool
	thinkOpen   map[string]bool
}

// NewStreamState tracks open message blocks for one SSE response.
func NewStreamState(threadID, runID string) *streamState {
	return &streamState{
		threadID:    threadID,
		runID:       runID,
		textStarted: map[string]bool{},
		thinkOpen:   map[string]bool{},
	}
}

// toAGUI maps one driver event (plus lifecycle state) to zero or one AG-UI
// events. Message START/END are synthesized around the first/last deltas.
func (st *streamState) toAGUI(ev driver.StreamEvent) agui.Event {
	switch v := ev.(type) {
	case driver.RunStarted:
		return agui.NewRunStartedEvent(st.threadID, st.runID)
	case driver.TextDelta:
		if !st.textStarted[v.MessageID] {
			st.textStarted[v.MessageID] = true
		}
		return agui.NewTextMessageContentEvent(v.MessageID, v.Text)
	case driver.ThinkingDelta:
		if !st.thinkOpen[v.MessageID] {
			st.thinkOpen[v.MessageID] = true
		}
		return agui.NewReasoningMessageContentEvent(v.MessageID, v.Text)
	case driver.ToolCallStart:
		return agui.NewToolCallStartEvent(v.ToolCallID, v.ToolCallName)
	case driver.ToolCallArgs:
		return agui.NewToolCallArgsEvent(v.ToolCallID, v.Delta)
	case driver.ToolCallEnd:
		return agui.NewToolCallEndEvent(v.ToolCallID)
	case driver.ToolCallResult:
		return agui.NewToolCallResultEvent(v.ToolCallID, v.ToolCallID, v.Content)
	case driver.RunFinished:
		return agui.NewRunFinishedEvent(st.threadID, st.runID)
	case driver.RunError:
		return agui.NewRunErrorEvent(v.Message)
	case driver.TerminalEvent:
		return nil
	default:
		return nil
	}
}
