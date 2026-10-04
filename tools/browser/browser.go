// Package browser wraps the agent-browser CLI (Chromium via CDP) as omp host
// tools. Tool results stream screenshots as ACTIVITY events on the AG-UI
// face and ordinary tool artifacts on A2A; user takeover is the
// browser_takeover / browser_hand_back tool pair.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
)

// Browser manages one agent-browser session for the whole process.
type Browser struct {
	mu      sync.Mutex
	session string // session name for this host (single agent per process)
	tmpDir  string
}

// New creates a browser tool backend.
func New() *Browser {
	tmp, err := os.MkdirTemp("", "agent-host-browser")
	if err != nil {
		tmp = os.TempDir()
	}
	return &Browser{session: "agent-host", tmpDir: tmp}
}

// Close drops the browser session and temp files.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.run(context.Background(), "close")
	_ = os.RemoveAll(b.tmpDir)
}

// run invokes one agent-browser command and returns its stdout.
func (b *Browser) run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--session", b.session}, args...)
	cmd := exec.CommandContext(ctx, "agent-browser", full...)
	out, err := cmd.Output()
	return string(out), err
}

// runJSON runs a command expecting JSON output.
func (b *Browser) runJSON(ctx context.Context, target any, args ...string) error {
	out, err := b.run(ctx, args...)
	if err != nil {
		return fmt.Errorf("agent-browser %s: %v: %s", strings.Join(args, " "), err, out)
	}
	if err := json.Unmarshal([]byte(out), target); err != nil {
		// some commands print plain text even with --json
		return fmt.Errorf("agent-browser %s: non-JSON: %s", strings.Join(args, " "), strings.TrimSpace(out))
	}
	return nil
}

// screenshotPath takes a screenshot into tmp and returns the path.
func (b *Browser) screenshotPath(ctx context.Context) (string, error) {
	timeNow++
	path := filepath.Join(b.tmpDir, fmt.Sprintf("shot-%d.png", timeNow))
	if _, err := b.run(ctx, "screenshot", path); err != nil {
		return "", err
	}
	return path, nil
}

// schemaObj builds a JSON Schema object parameter map (omp Parameters is a
// JSON Schema object of raw values).
func schemaObj(extra ...string) map[string]json.RawMessage {
	m := map[string]json.RawMessage{"type": json.RawMessage(`"object"`)}
	for _, e := range extra {
		key, val, ok := strings.Cut(e, ":")
		if ok {
			m[strings.Trim(key, `"`)] = json.RawMessage(strings.TrimSpace(val))
		}
	}
	return m
}

// HostTools returns the omp host tools for the browser.
func (b *Browser) HostTools() []omprpc.HostTool {
	return []omprpc.HostTool{
		{
			Definition: omprpc.HostToolDefinition{
				Name:        "browser_open",
				Description: "Navigate the browser to a URL.",
				Parameters: schemaObj(
					`"properties": {"url": {"type": "string"}}`,
					`"required": ["url"]`,
				),
				LoadMode: omprpc.Ptr(omprpc.ToolLoadModeEssential),
			},
			Handler: b.handleOpen,
		},
		{
			Definition: omprpc.HostToolDefinition{
				Name:        "browser_snapshot",
				Description: "Return the accessibility snapshot (interactive elements, with @refs) of the current page.",
				Parameters:  schemaObj(),
				LoadMode:    omprpc.Ptr(omprpc.ToolLoadModeEssential),
			},
			Handler: b.handleSnapshot,
		},
		{
			Definition: omprpc.HostToolDefinition{
				Name:        "browser_click",
				Description: "Click an element by @ref (from browser_snapshot) or CSS selector.",
				Parameters: schemaObj(
					`"properties": {"ref": {"type": "string"}}`,
					`"required": ["ref"]`,
				),
				LoadMode: omprpc.Ptr(omprpc.ToolLoadModeEssential),
			},
			Handler: b.handleClick,
		},
		{
			Definition: omprpc.HostToolDefinition{
				Name:        "browser_fill",
				Description: "Clear and fill a form field by @ref or selector.",
				Parameters: schemaObj(
					`"properties": {"ref": {"type": "string"}, "text": {"type": "string"}}`,
					`"required": ["ref", "text"]`,
				),
				LoadMode: omprpc.Ptr(omprpc.ToolLoadModeEssential),
			},
			Handler: b.handleFill,
		},
		{
			Definition: omprpc.HostToolDefinition{
				Name:        "browser_screenshot",
				Description: "Capture the current page as a PNG screenshot; returns the file path for the host to attach.",
				Parameters:  schemaObj(`"properties": {"full": {"type": "boolean"}}`),
				LoadMode:    omprpc.Ptr(omprpc.ToolLoadModeDiscoverable),
			},
			Handler: b.handleScreenshot,
		},
		{
			Definition: omprpc.HostToolDefinition{
				Name:        "browser_takeover",
				Description: "Hand interactive browser control to the user; the agent pauses browser actions until browser_hand_back.",
				Parameters:  schemaObj(),
				LoadMode:    omprpc.Ptr(omprpc.ToolLoadModeDiscoverable),
			},
			Handler: b.handleTakeover,
		},
		{
			Definition: omprpc.HostToolDefinition{
				Name:        "browser_hand_back",
				Description: "Resume agent control of the browser after user takeover.",
				Parameters:  schemaObj(),
				LoadMode:    omprpc.Ptr(omprpc.ToolLoadModeDiscoverable),
			},
			Handler: b.handleHandBack,
		},
	}
}

func (b *Browser) handleOpen(ctx context.Context, call *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return omprpc.HostToolResultPayload{}, err
	}
	out, err := b.run(ctx, "open", p.URL)
	if err != nil {
		return omprpc.TextResult("open failed: " + strings.TrimSpace(out)), nil
	}
	return omprpc.TextResult("opened " + p.URL), nil
}

func (b *Browser) handleSnapshot(ctx context.Context, call *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
	out, err := b.run(ctx, "snapshot", "-i")
	if err != nil {
		return omprpc.TextResult("snapshot failed: " + strings.TrimSpace(out)), nil
	}
	return omprpc.TextResult(strings.TrimSpace(out)), nil
}

func (b *Browser) handleClick(ctx context.Context, call *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
	var p struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return omprpc.HostToolResultPayload{}, err
	}
	out, err := b.run(ctx, "click", p.Ref)
	if err != nil {
		return omprpc.TextResult("click failed: " + strings.TrimSpace(out)), nil
	}
	return omprpc.TextResult("clicked " + p.Ref), nil
}

func (b *Browser) handleFill(ctx context.Context, call *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
	var p struct {
		Ref  string `json:"ref"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return omprpc.HostToolResultPayload{}, err
	}
	out, err := b.run(ctx, "fill", p.Ref, p.Text)
	if err != nil {
		return omprpc.TextResult("fill failed: " + strings.TrimSpace(out)), nil
	}
	return omprpc.TextResult("filled " + p.Ref), nil
}

func (b *Browser) handleScreenshot(ctx context.Context, call *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
	var p struct {
		Full bool `json:"full"`
	}
	_ = json.Unmarshal(args, &p) // optional
	path, err := b.screenshotPath(ctx)
	if err != nil {
		return omprpc.TextResult("screenshot failed: " + err.Error()), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return omprpc.TextResult("screenshot read failed: " + err.Error()), nil
	}
	payload := omprpc.HostToolResultPayload{
		Content: []omprpc.UserContent{{Value: omprpc.ImageContent{
			Data:     base64Encode(data),
			MimeType: "image/png",
		}}},
	}
	return payload, nil
}

func (b *Browser) handleTakeover(ctx context.Context, call *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
	// User takeover: relaunch the browser headed so the user can interact.
	_, _ = b.run(ctx, "close")
	out, err := b.run(ctx, "open", "--headed", "about:blank")
	if err != nil {
		return omprpc.TextResult("takeover failed: " + strings.TrimSpace(out)), nil
	}
	return omprpc.TextResult("browser handed to user; use browser_hand_back to resume agent control"), nil
}

func (b *Browser) handleHandBack(ctx context.Context, call *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
	_, _ = b.run(ctx, "close")
	return omprpc.TextResult("agent control resumed"), nil
}

// timeNow is the screenshot counter.
var timeNow int64

// base64Encode encodes bytes to standard base64.
func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
