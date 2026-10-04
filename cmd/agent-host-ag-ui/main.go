// Command agent-host-ag-ui serves all faces of the agent host in one
// process: AG-UI HTTP/SSE (:8090), A2A gRPC (:80), A2A HTTP-JSON (:8091),
// /readyz (:8081), /healthz, and the test GUI.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/brendanjerwin/agent-host-ag-ui/internal/a2a"
	ompdriver "github.com/brendanjerwin/agent-host-ag-ui/internal/driver/omp"
	"github.com/brendanjerwin/agent-host-ag-ui/internal/server"
	"github.com/brendanjerwin/agent-host-ag-ui/tools/browser"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	sessionRoot := envOr("AGENT_HOST_AG_UI_SESSION_ROOT", filepath.Join(os.Getenv("HOME"), ".agent-host-ag-ui", "sessions"))
	browser := browser.New()
	defer browser.Close()

	d, err := ompdriver.New(ompdriver.Options{
		Bin:         envOr("AGENT_HOST_AG_UI_OMP_BIN", "omp"),
		SessionRoot: sessionRoot,
		Cwd:         envOr("AGENT_HOST_AG_UI_CWD", ""),
		HostTools:   browser.HostTools(),
	})
	if err != nil {
		slog.Error("driver init", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// AG-UI face.
	srv := &server.Server{Driver: d, Cfg: server.Config{Addr: envOr("AGENT_HOST_AG_UI_ADDR", ":8090")}}
	go func() {
		slog.Info("ag-ui listening", "addr", srv.Cfg.Addr)
		if err := http.ListenAndServe(srv.Cfg.Addr, srv.Handler()); err != nil {
			slog.Error("ag-ui server", "err", err)
			os.Exit(1)
		}
	}()

	// A2A face (byo contract).
	face := a2a.NewFace(d)
	if err := a2a.Serve(
		ctx, face,
		envOr("AGENT_HOST_AG_UI_A2A_ADDR", ":80"),
		envOr("AGENT_HOST_AG_UI_A2A_HTTP_ADDR", ":8091"),
		envOr("AGENT_HOST_AG_UI_READYZ_ADDR", ":8081"),
		d.Health,
	); err != nil {
		slog.Error("a2a server", "err", err)
		os.Exit(1)
	}

	if err := d.Close(); err != nil {
		slog.Warn("driver close", "err", err)
	}
}
