// Package server serves the AG-UI face of agent-host-ag-ui over HTTP/SSE.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/encoding/sse"
	"github.com/brendanjerwin/agent-host-ag-ui/internal/driver"
)

//go:embed static/index.html
var staticFS embed.FS

// Config for the HTTP server.
type Config struct {
	Addr string // AG-UI listen addr (default :8090)
}

// Server exposes POST /ag-ui (SSE), GET /healthz, and the test GUI at /.
type Server struct {
	Driver driver.Driver
	Cfg    Config
}

// RunAgentInput mirrors the AG-UI run input (mirror structs, strict
// validation: 400 on malformed JSON / missing threadId / empty messages /
// missing user-message tail).
type RunAgentInput struct {
	ThreadID string          `json:"threadId"`
	RunID    string          `json:"runId"`
	State    any             `json:"state"`
	Messages []types.Message `json:"messages"`
	Tools    []types.Tool    `json:"tools"`
	Context  []types.Context `json:"context"`
}

// validate enforces the 400 contract.
func (r *RunAgentInput) validate() error {
	if r.ThreadID == "" {
		return errors.New("threadId is required")
	}
	if len(r.Messages) == 0 {
		return errors.New("messages must not be empty")
	}
	last := r.Messages[len(r.Messages)-1]
	if string(last.Role) != "user" {
		return errors.New("last message must have role 'user'")
	}
	if last.Content == nil {
		return errors.New("last message must have content")
	}
	return nil
}

// userText extracts plain text from the last user message content.
func userText(m types.Message) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var b []byte
		for _, part := range c {
			if pm, ok := part.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					b = append(b, t...)
				}
			}
		}
		return string(b)
	default:
		if c == nil {
			return ""
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ag-ui", s.handleAGUI)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleHealth)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

// handleHealth returns 200 when the omp binary resolves.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if h, ok := s.Driver.(interface{ Health() error }); ok && h.Health() != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleIndex serves the embedded test GUI.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, _ := staticFS.ReadFile("static/index.html")
	_, _ = w.Write(data)
}

// handleAGUI validates input then streams AG-UI events as SSE.
func (s *Server) handleAGUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed; POST /ag-ui", http.StatusMethodNotAllowed)
		return
	}
	var input RunAgentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"malformed JSON"}`, http.StatusBadRequest)
		return
	}
	if err := input.validate(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":%q}`, err.Error())
		return
	}

	req := driver.RunRequest{
		ThreadID:    input.ThreadID,
		RunID:       input.RunID,
		UserMessage: userText(input.Messages[len(input.Messages)-1]),
	}
	events, err := s.Driver.Stream(r.Context(), req)
	if err != nil {
		if errors.Is(err, driver.ErrRunInFlight) {
			http.Error(w, `{"error":"run already in flight for thread"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	writer := sse.NewSSEWriter()
	state := NewStreamState(req.ThreadID, req.RunID)

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()
	idle := true
	for {
		select {
		case <-ctx.Done():
			// client abort: cancel driver-side and drain
			_ = s.Driver.Cancel(req.ThreadID, req.RunID)
			return
		case ev, open := <-events:
			if !open {
				return
			}
			idle = false
			if _, isTerm := ev.(driver.TerminalEvent); isTerm {
				return
			}
			aguiEvent := state.toAGUI(ev)
			if aguiEvent == nil {
				continue
			}
			if err := writer.WriteEventWithType(ctx, w, aguiEvent, string(aguiEventType(ev))); err != nil {
				slog.Warn("agui write failed", "err", err)
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if idle {
				_, _ = fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}
