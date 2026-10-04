package a2a

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/brendanjerwin/agent-host-ag-ui/internal/driver"
)

// fakeDriver replays canned driver events for one thread.
type fakeDriver struct {
	events  []driver.StreamEvent
	runs    []driver.RunRequest
	cancels []string
}

func (f *fakeDriver) Stream(ctx context.Context, req driver.RunRequest) (<-chan driver.StreamEvent, error) {
	f.runs = append(f.runs, req)
	out := make(chan driver.StreamEvent, len(f.events)+1)
	for _, ev := range f.events {
		out <- ev
	}
	out <- driver.TerminalEvent{Terminal: driver.TerminalFinished}
	close(out)
	return out, nil
}

func (f *fakeDriver) Cancel(threadID, runID string) error {
	f.cancels = append(f.cancels, threadID+"/"+runID)
	return nil
}

func (f *fakeDriver) Close() error { return nil }

func TestExecutorTaskFlow(t *testing.T) {
	fd := &fakeDriver{
		events: []driver.StreamEvent{
			driver.RunStarted{ThreadID: "ctx1", RunID: "task1"},
			driver.TextDelta{MessageID: "m1", Text: "hello"},
			driver.RunFinished{ThreadID: "ctx1", RunID: "task1"},
		},
	}
	f := NewFace(fd)
	exec := f.Executor()
	ec := &a2asrv.ExecutorContext{
		TaskID:    "task1",
		ContextID: "ctx1",
		Message: &a2a.Message{
			Role:  a2a.MessageRoleUser,
			ID:    "msg1",
			Parts: []*a2a.Part{a2a.NewTextPart("say hi")},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []string
	for ev, err := range exec.Execute(ctx, ec) {
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
		switch v := ev.(type) {
		case *a2a.Task:
			got = append(got, "task:"+string(v.Status.State))
		case *a2a.TaskStatusUpdateEvent:
			got = append(got, "status:"+string(v.Status.State))
		case *a2a.TaskArtifactUpdateEvent:
			for _, p := range v.Artifact.Parts {
				got = append(got, "artifact:"+p.Text())
			}
		default:
			got = append(got, "other")
		}
	}
	want := []string{
		"task:TASK_STATE_SUBMITTED",
		"status:TASK_STATE_WORKING",
		"artifact:hello",
		"status:TASK_STATE_COMPLETED",
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %s != %s", i, got[i], want[i])
		}
	}
	if len(fd.runs) != 1 || fd.runs[0].ThreadID != "ctx1" || fd.runs[0].RunID != "task1" || fd.runs[0].UserMessage != "say hi" {
		t.Errorf("driver request: %#v", fd.runs[0])
	}
}

func TestExecutorErrorFlow(t *testing.T) {
	fd := &fakeDriver{
		events: []driver.StreamEvent{
			driver.RunError{ThreadID: "ctx1", RunID: "task1", Message: "boom"},
		},
	}
	f := NewFace(fd)
	exec := f.Executor()
	ec := &a2asrv.ExecutorContext{
		TaskID:    "task1",
		ContextID: "ctx1",
		Message: &a2a.Message{
			Role:  a2a.MessageRoleUser,
			ID:    "msg1",
			Parts: []*a2a.Part{a2a.NewTextPart("x")},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	states := []string{}
	for ev, err := range exec.Execute(ctx, ec) {
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
		if sue, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
			states = append(states, string(sue.Status.State))
		}
	}
	if len(states) == 0 || states[len(states)-1] != string(a2a.TaskStateFailed) {
		t.Fatalf("states: %#v", states)
	}
}

func TestExecutorCancel(t *testing.T) {
	fd := &fakeDriver{}
	f := NewFace(fd)
	exec := f.Executor()
	ec := &a2asrv.ExecutorContext{TaskID: "task1", ContextID: "ctx1"}
	for _, err := range exec.Cancel(context.Background(), ec) {
		if err != nil {
			t.Fatalf("cancel error: %v", err)
		}
	}
	if len(fd.cancels) != 1 || fd.cancels[0] != "ctx1/task1" {
		t.Fatalf("cancels: %#v", fd.cancels)
	}
}

// compile-time: executor satisfies the interface
var _ a2asrv.AgentExecutor = (*executor)(nil)
