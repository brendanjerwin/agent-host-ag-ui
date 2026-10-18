package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/a2aproject/a2a-go/v2/a2a"
	kagentapp "github.com/kagent-dev/kagent/go/adk/pkg/app"
)

// ServeKagent runs the A2A face through kagent's own ADK app helper: the
// task store is backed by the controller's private gRPC persistence service
// (KAGENT_API_URL), the runtime identity is the file Substrate projects at
// /run/kagent/identity/name, and the server owns gRPC :port + /readyz :8081.
// The AG-UI face is served by main separately. Blocks until shutdown.
func ServeKagent(ctx context.Context, f *Face, port string) error {
	card := f.Card
	if raw := os.Getenv("KAGENT_AGENT_CARD_JSON"); raw != "" {
		parsed := &a2a.AgentCard{}
		if err := json.Unmarshal([]byte(raw), parsed); err == nil {
			card = parsed
		}
	}

	application, err := kagentapp.New(kagentapp.AppConfig{
		AgentCard: *card,
		Port:      port,
		AppName:   "agent-host-ag-ui",
	}, f.Executor())
	if err != nil {
		return fmt.Errorf("a2a: kagent app: %w", err)
	}
	return application.Run()
}

// KagentManaged reports whether this run is kagent-managed (byo actor):
// the controller injects KAGENT_API_URL alongside KAGENT_CONFIG_JSON.
func KagentManaged() bool { return os.Getenv("KAGENT_API_URL") != "" }
