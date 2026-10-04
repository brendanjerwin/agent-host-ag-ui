// Package omp implements the driver.Driver interface against a long-lived
// `omp --mode rpc` child process per thread, connected with the official
// omprpc SDK. Sessions persist in per-thread directories under SessionRoot,
// so threads resume across adapter restarts (omp session-dir resume).
package omp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/brendanjerwin/agent-host-ag-ui/internal/driver"
	"github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
	"github.com/google/uuid"
)

// Options configures the omp child environment.
type Options struct {
	// Bin is the omp binary path (default "omp").
	Bin string
	// SessionRoot is the parent directory of per-thread session dirs.
	SessionRoot string
	// Cwd is the working directory for omp children.
	Cwd string
	// HostTools are registered with every child (e.g. the browser tool).
	HostTools []omprpc.HostTool
	// Stderr receives child stderr (nil discards).
	Stderr *os.File
}

// threadDirPattern guards the on-disk name after SanitizeThreadID.
var threadDirPattern = regexp.MustCompile(`^(?:[a-z0-9-]{1,64}|hex-[0-9a-f]{1,256})$`)

// Driver runs omp children. Create with New; one Driver for the process.
type Driver struct {
	opts Options

	mu      sync.Mutex
	closed  bool
	clients map[string]*threadState // sanitized threadID -> state
}

type threadState struct {
	client *omprpc.Client
	dir    string

	// Exactly one in-flight run per thread.
	runMu    sync.Mutex
	runCtx   context.Context
	runID    string
	cancelFn context.CancelFunc
	done     chan struct{}
}

// New validates options and prepares the session root.
func New(opts Options) (*Driver, error) {
	if opts.Bin == "" {
		opts.Bin = "omp"
	}
	if opts.SessionRoot == "" {
		return nil, errors.New("omp: SessionRoot is required")
	}
	if err := os.MkdirAll(opts.SessionRoot, 0o755); err != nil {
		return nil, fmt.Errorf("omp: session root: %w", err)
	}
	return &Driver{opts: opts, clients: make(map[string]*threadState)}, nil
}

// Health reports whether the omp binary resolves (used by /healthz).
func (d *Driver) Health() error {
	_, err := exec.LookPath(d.opts.Bin)
	return err
}

// threadDir computes and validates the session directory for a thread id.
func (d *Driver) threadDir(threadID string) (string, error) {
	safe := driver.SanitizeThreadID(threadID)
	if !threadDirPattern.MatchString(safe) {
		return "", fmt.Errorf("omp: unsafe thread id %q", threadID)
	}
	return filepath.Join(d.opts.SessionRoot, safe), nil
}

// Close tears down every live child. Safe for concurrent use.
func (d *Driver) Close() error {
	d.mu.Lock()
	d.closed = true
	states := make([]*threadState, 0, len(d.clients))
	for _, st := range d.clients {
		states = append(states, st)
	}
	d.clients = map[string]*threadState{}
	d.mu.Unlock()
	for _, st := range states {
		st.shutdown()
	}
	return nil
}

// Cancel aborts the in-flight run for a thread/run pair.
func (d *Driver) Cancel(threadID, runID string) error {
	d.mu.Lock()
	st := d.clients[driver.SanitizeThreadID(threadID)]
	d.mu.Unlock()
	if st == nil {
		return driver.ErrRunNotFound
	}
	st.runMu.Lock()
	defer st.runMu.Unlock()
	if st.cancelFn == nil || (runID != "" && st.runID != runID) {
		return driver.ErrRunNotFound
	}
	st.cancelFn()
	return nil
}

// shutdown tears down one thread's child: cancels the in-flight run, then
// closes the omprpc client (SIGTERM group, bounded SIGKILL per SDK).
func (st *threadState) shutdown() {
	st.runMu.Lock()
	if st.cancelFn != nil {
		st.cancelFn()
	}
	runDone := st.done
	st.runMu.Unlock()
	if runDone != nil {
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			// The stream goroutine leaked past cancellation; still close the
			// transport so the child dies.
		}
	}
	_ = st.client.Close()
}

// Stream runs one agent turn on the thread's omp child and streams
// driver.StreamEvents until terminal. Run completion follows the omp prompt
// flow: live frames until prompt_result (request id matched) and agent_end.
func (d *Driver) Stream(ctx context.Context, req driver.RunRequest) (<-chan driver.StreamEvent, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, driver.ErrDriverClosed
	}
	threadID := driver.SanitizeThreadID(req.ThreadID)
	st := d.clients[threadID]
	if st == nil {
		dir, err := d.threadDir(req.ThreadID)
		if err != nil {
			d.mu.Unlock()
			return nil, err
		}
		client, err := d.spawn(ctx, dir)
		if err != nil {
			d.mu.Unlock()
			return nil, err
		}
		st = &threadState{client: client, dir: dir}
		d.clients[threadID] = st
	}
	d.mu.Unlock()

	// One in-flight run per thread: claim under runMu.
	st.runMu.Lock()
	if st.cancelFn != nil {
		st.runMu.Unlock()
		return nil, driver.ErrRunInFlight
	}
	runID := req.RunID
	if runID == "" {
		runID = uuid.NewString()
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// Keep the request context's cancellation (client disconnect) but make
	// the run context independent of ctx values.
	cancelOwned := cancel
	st.runID = runID
	st.cancelFn = cancelOwned
	st.done = make(chan struct{})
	done := st.done
	st.runMu.Unlock()

	out := make(chan driver.StreamEvent, 64)
	go func() {
		defer close(done)
		defer close(out)
		defer func() {
			st.runMu.Lock()
			st.cancelFn = nil
			st.runID = ""
			st.runMu.Unlock()
		}()
		d.streamRun(runCtx, st, req, runID, out)
	}()
	return out, nil
}

// spawn launches a fresh omp child attached to the thread session dir.
func (d *Driver) spawn(ctx context.Context, dir string) (*omprpc.Client, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("omp: thread dir: %w", err)
	}
	cmd := exec.Command(d.opts.Bin, "--mode", "rpc", "--no-ui", "--session-dir", dir)
	cmd.Dir = d.opts.Cwd
	if d.opts.Stderr != nil {
		cmd.Stderr = d.opts.Stderr
	}
	opts := append([]omprpc.Option{}, hostToolOptions(d.opts.HostTools)...)
	client, err := omprpc.Start(ctx, cmd, opts...)
	if err != nil {
		return nil, fmt.Errorf("omp: start child: %w", err)
	}
	return client, nil
}

// hostToolOptions wraps host tools as omprpc options.
func hostToolOptions(tools []omprpc.HostTool) []omprpc.Option {
	if len(tools) == 0 {
		return nil
	}
	return []omprpc.Option{omprpc.WithHostTools(tools...)}
}

// streamRun sends the prompt and translates live frames until terminal.
func (d *Driver) streamRun(
	runCtx context.Context,
	st *threadState,
	req driver.RunRequest,
	runID string,
	out chan<- driver.StreamEvent,
) {
	// RUN_STARTED is the first event of every run, before driver I/O.
	if !emit(runCtx, out, driver.RunStarted{ThreadID: req.ThreadID, RunID: runID}) {
		return
	}
	reqID := "run_" + runID
	promptCtx := omprpc.WithRequestID(runCtx, reqID)
	if _, err := st.client.Prompt(promptCtx, omprpc.PromptCommand{Message: req.UserMessage}); err != nil {
		if errors.Is(err, context.Canceled) || runCtx.Err() != nil {
			emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalAborted})
			return
		}
		emit(runCtx, out, driver.RunError{ThreadID: req.ThreadID, RunID: runID, Message: err.Error()})
		emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalError})
		return
	}

	tr := newTranslator(req.ThreadID, runID)
	frames := st.client.Frames()
	for {
		select {
		case <-runCtx.Done():
			// Client disconnect or Cancel: abort omp server-side, drain
			// until the abort lands, then close without wire emission.
			_, _ = st.client.Call(context.Background(), "abort", nil, 5*time.Second)
			emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalAborted})
			return
		case frame, open := <-frames:
			if !open {
				// Child died mid-run.
				if runCtx.Err() != nil {
					emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalAborted})
					return
				}
				emit(runCtx, out, driver.RunError{ThreadID: req.ThreadID, RunID: runID, Message: "omp child exited"})
				emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalError})
				return
			}
			terminal, status := tr.translate(runCtx, frame, out)
			switch terminal {
			case translatorDone:
				if status == "error" {
					emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalError})
				} else if runCtx.Err() != nil {
					emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalAborted})
				} else {
					emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalFinished})
				}
				return
			case translatorAborted:
				emit(runCtx, out, driver.TerminalEvent{Terminal: driver.TerminalAborted})
				return
			}
		}
	}
}

// emit sends one event unless the run context is done.
func emit(ctx context.Context, out chan<- driver.StreamEvent, ev driver.StreamEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// SanitizeThreadID re-exported for callers that normalize ids before storage.
func SanitizeThreadID(id string) string { return driver.SanitizeThreadID(id) }
