// Package driver defines the transport-agnostic agent execution seam shared
// by the AG-UI adapter face and the A2A face. Both faces stream the same
// driver events; the AG-UI translator (internal/driver/omp) converts them to
// AG-UI protocol events, the A2A shim maps them to A2A task events.
package driver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// RunRequest is one agent run keyed by an externally supplied thread/run id.
// ThreadID is the AG-UI threadId / A2A contextId; it names the omp session
// directory on disk and must survive process restarts.
type RunRequest struct {
	ThreadID    string // AG-UI threadId / A2A contextId; sanitized by SanitizeThreadID
	RunID       string // AG-UI runId / A2A task id; caller mints a UUID when absent
	UserMessage string // last user message text from the protocol input
}

// RunInfo reports an in-flight run registered under a thread.
type RunInfo struct {
	RunID string
	// Cancel aborts the run; safe to call more than once and after completion.
	Cancel context.CancelFunc
}

// ErrRunInFlight reports that the thread already has an active run. Callers
// surface it as HTTP 409 (AG-UI) or task rejection (A2A): one in-flight run
// per thread, no queueing.
var ErrRunInFlight = errors.New("driver: thread already has a run in flight")

// ErrRunNotFound reports that no in-flight run matches the given thread/run.
var ErrRunNotFound = errors.New("driver: no in-flight run for thread/run")

// ErrDriverClosed reports operations attempted after Close.
var ErrDriverClosed = errors.New("driver: closed")

// threadIDPattern is the allowlist for session-directory names. Anything not
// matching is hex-wrapped by SanitizeThreadID (traversal defense: the id
// becomes a directory name under SessionRoot).
var threadIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// SanitizeThreadID maps a caller-supplied thread id to a safe session
// directory name. Well-formed ids pass through unchanged; anything else is
// hex-wrapped deterministically so the same id resumes the same session.
func SanitizeThreadID(id string) string {
	if threadIDPattern.MatchString(id) {
		return id
	}
	return fmt.Sprintf("hex-%x", []byte(id))
}

// Driver executes agent runs as event streams. Implementations must be safe
// for concurrent use; exactly one in-flight run is allowed per thread.
type Driver interface {
	// Stream begins a run. The returned channel is closed when the run
	// reaches a terminal event (finished or error) or the context is
	// canceled. The first event delivered is always RUN_STARTED.
	Stream(ctx context.Context, req RunRequest) (<-chan StreamEvent, error)
	// Cancel aborts the in-flight run for a thread. It returns
	// ErrRunNotFound when no run matches. The run's channel still closes
	// normally (terminal event or clean close) after cancellation.
	Cancel(threadID, runID string) error
	// Close tears down all live agent sessions. It blocks until every run
	// channel is closed and every child process is gone.
	Close() error
}

// StreamTerminal distinguishes how a stream ended.
type StreamTerminal string

const (
	// TerminalFinished: the run completed; a FINISHED event was emitted.
	TerminalFinished StreamTerminal = "finished"
	// TerminalError: the run failed; an ERROR event was emitted.
	TerminalError StreamTerminal = "error"
	// TerminalAborted: the client disconnected or Cancel ran; no wire
	// emission is owed (AG-UI: SSE breaks; A2A: task cancel state).
	TerminalAborted StreamTerminal = "aborted"
)

// StreamEvent is one protocol-neutral event in a run's stream. Concrete
// payload types below; `Terminal` values ride in a TerminalEvent.
type StreamEvent interface{ isStreamEvent() }

// TerminalEvent is the last event of every stream. Terminal Aborted means
// the client disconnected or Cancel ran: no wire emission is owed.
type TerminalEvent struct{ Terminal StreamTerminal }

func (TerminalEvent) isStreamEvent() {}

// RunStarted is the first event of every run.
type RunStarted struct{ ThreadID, RunID string }

func (RunStarted) isStreamEvent() {}

// TextDelta appends text to the current assistant message.
type TextDelta struct {
	MessageID string // stable omp messageId for the message block
	Text      string
}

func (TextDelta) isStreamEvent() {}

// ThinkingDelta appends reasoning text to the current reasoning block.
// Reasoning messages use distinct messageIds from visible text.
type ThinkingDelta struct {
	MessageID string
	Text      string
}

func (ThinkingDelta) isStreamEvent() {}

// ToolCallStart opens a tool call invocation.
type ToolCallStart struct {
	ToolCallID   string // omp toolCallId (from toolcall_end's ToolCall.ID when needed)
	ToolCallName string
	MessageID    string // assistant message the call belongs to
}

func (ToolCallStart) isStreamEvent() {}

// ToolCallArgs streams one fragment of tool call arguments.
type ToolCallArgs struct {
	ToolCallID string
	Delta      string
}

func (ToolCallArgs) isStreamEvent() {}

// ToolCallEnd closes a tool call invocation.
type ToolCallEnd struct {
	ToolCallID string
}

func (ToolCallEnd) isStreamEvent() {}

// ToolCallResult delivers the tool's output for a call.
type ToolCallResult struct {
	ToolCallID string
	ToolName   string
	Content    string // textual content (large binary payloads are truncated)
	IsError    bool
}

func (ToolCallResult) isStreamEvent() {}

// RunFinished is the successful terminal event.
type RunFinished struct {
	ThreadID string
	RunID    string
}

func (RunFinished) isStreamEvent() {}

// RunError is the failure terminal event; no FINISHED follows.
type RunError struct {
	ThreadID string
	RunID    string
	Message  string
}

func (RunError) isStreamEvent() {}
