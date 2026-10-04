// Package a2a implements the A2A face of agent-host-ag-ui: an
// a2asrv.AgentExecutor backed by the shared driver, serving the kagent byo
// contract (gRPC lf.a2a.v1.A2AService on :80, /readyz on :8081).
package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/brendanjerwin/agent-host-ag-ui/internal/driver"
)

// Face wraps the driver for A2A task execution.
type Face struct {
	Driver driver.Driver
	// Card is the AgentCard to serve when KAGENT_AGENT_CARD_JSON is absent.
	Card *a2a.AgentCard
}

// NewFace builds an A2A Face over the driver.
func NewFace(d driver.Driver) *Face {
	return &Face{
		Driver: d,
		Card: &a2a.AgentCard{
			Name:        "agent-host-ag-ui",
			Description: "omp coding-agent sessions over AG-UI and A2A",
			SupportedInterfaces: []*a2a.AgentInterface{
				a2a.NewAgentInterface(":"+os.Getenv("AGENT_HOST_AG_UI_A2A_ADDR"), a2a.TransportProtocolGRPC),
			},
			DefaultInputModes:  []string{"text"},
			DefaultOutputModes: []string{"text"},
			Capabilities:       a2a.AgentCapabilities{Streaming: true},
			Skills: []a2a.AgentSkill{
				{ID: "prompt-execution", Name: "Prompt execution", Description: "Run an omp coding-agent turn", Tags: []string{"coding-agent"}},
				{ID: "streaming", Name: "Streaming updates", Description: "Stream task artifacts while working", Tags: []string{"streaming"}},
				{ID: "cancel", Name: "Cancellation", Description: "Cancel an in-flight run", Tags: []string{"cancel"}},
			},
		},
	}
}

// AgentCard returns the configured card: KAGENT_AGENT_CARD_JSON when
// injected (kagent byo contract), else the embedded default.
func (f *Face) AgentCard(ctx context.Context) (*a2a.AgentCard, error) {
	if raw := os.Getenv("KAGENT_AGENT_CARD_JSON"); raw != "" {
		card := &a2a.AgentCard{}
		if err := json.Unmarshal([]byte(raw), card); err == nil {
			return card, nil
		}
	}
	return f.Card, nil
}

// Executor returns the AgentExecutor wired to the driver.
func (f *Face) Executor() a2asrv.AgentExecutor { return &executor{face: f} }

// executor adapts driver streams to a2a event iterators.
type executor struct{ face *Face }

// Execute runs the driver stream and yields A2A events:
// submitted task -> working status -> text artifacts per message -> terminal.
func (e *executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		msg := execCtx.Message
		if msg == nil {
			yield(nil, fmt.Errorf("a2a: cancel-style request routed to Execute"))
			return
		}
		text := messageText(msg)
		threadID := execCtx.ContextID
		if threadID == "" {
			threadID = string(execCtx.TaskID)
		}
		req := driver.RunRequest{
			ThreadID:    threadID,
			RunID:       string(execCtx.TaskID),
			UserMessage: text,
		}
		events, err := e.face.Driver.Stream(ctx, req)
		if err != nil {
			yield(nil, err)
			return
		}
		// Submitted task first (per template), then stream artifacts.
		if !yield(a2a.NewSubmittedTask(execCtx, msg), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		var artifactID a2a.ArtifactID
		for ev := range events {
			select {
			case <-ctx.Done():
				_ = e.face.Driver.Cancel(req.ThreadID, req.RunID)
				return
			default:
			}
			switch v := ev.(type) {
			case driver.TextDelta:
				// one artifact per assistant message; first delta creates it
				if artifactID == "" {
					evt := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(v.Text))
					artifactID = evt.Artifact.ID
					if !yield(evt, nil) {
						return
					}
				} else {
					evt := a2a.NewArtifactUpdateEvent(execCtx, artifactID, a2a.NewTextPart(v.Text))
					if !yield(evt, nil) {
						return
					}
				}
			case driver.ToolCallStart:
				// tool progress surfaces as a distinct artifact
				evt := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(
					fmt.Sprintf("tool call: %s (%s)", v.ToolCallName, v.ToolCallID),
				))
				if !yield(evt, nil) {
					return
				}
			case driver.ToolCallResult:
				content := v.Content
				if v.IsError {
					content = "ERROR: " + content
				}
				evt := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(content))
				if !yield(evt, nil) {
					return
				}
			case driver.RunError:
				errMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(v.Message))
				if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, errMsg), nil) {
					return
				}
				return
			case driver.RunFinished:
				if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil) {
					return
				}
				return
			case driver.TerminalEvent:
				switch v.Terminal {
				case driver.TerminalFinished:
					if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil) {
					}
					return
				case driver.TerminalError:
					errMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("run failed"))
					_ = yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, errMsg), nil)
					return
				case driver.TerminalAborted:
					_ = yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
					return
				}
			}
		}
	}
}

// Cancel implements the cancel path: driver cancel + canceled state.
func (e *executor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		threadID := execCtx.ContextID
		if threadID == "" {
			threadID = string(execCtx.TaskID)
		}
		_ = e.face.Driver.Cancel(threadID, string(execCtx.TaskID))
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// messageText extracts concatenated text parts of a message.
func messageText(m *a2a.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p == nil {
			continue
		}
		b.WriteString(p.Text())
	}
	return b.String()
}

// ReadyzOK reports driver health for the /readyz handler.
func (f *Face) ReadyzOK(health func() error) bool { return health() == nil }
