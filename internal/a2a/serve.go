package a2a

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

// Serve runs the A2A face: gRPC A2AService on grpcAddr and the public Agent
// Card endpoint. readyzAddr additionally serves GET /readyz (200 = healthy).
// All three share the Face; the call blocks until a server fails.
func Serve(ctx context.Context, f *Face, grpcAddr, cardAddr, readyzAddr string, health func() error) error {
	card, err := f.AgentCard(ctx)
	if err != nil {
		return fmt.Errorf("a2a: agent card: %w", err)
	}

	requestHandler := a2asrv.NewHandler(
		f.Executor(),
		a2asrv.WithExtendedAgentCard(card),
	)

	g, ctx := errgroup.WithContext(ctx)

	// gRPC A2AService (kagent byo contract dials this).
	g.Go(func() error {
		lis, err := net.Listen("tcp", grpcAddr)
		if err != nil {
			return fmt.Errorf("a2a: grpc listen %s: %w", grpcAddr, err)
		}
		s := grpc.NewServer()
		a2agrpc.NewHandler(requestHandler).RegisterWith(s)
		slog.Info("a2a grpc listening", "addr", grpcAddr)
		go func() {
			<-ctx.Done()
			s.Stop()
		}()
		return s.Serve(lis)
	})

	// Public AgentCard (well-known path) + HTTP-JSON binding on the card port.
	g.Go(func() error {
		lis, err := net.Listen("tcp", cardAddr)
		if err != nil {
			return fmt.Errorf("a2a: card listen %s: %w", cardAddr, err)
		}
		mux := http.NewServeMux()
		mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
		mux.Handle("/", a2asrv.NewJSONRPCHandler(requestHandler))
		srv := &http.Server{Handler: mux}
		slog.Info("a2a http listening", "addr", cardAddr)
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		return srv.Serve(lis)
	})

	// /readyz on its own port (kagent contract).
	g.Go(func() error {
		mux := http.NewServeMux()
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			if health == nil || health() != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"error"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		srv := &http.Server{Addr: readyzAddr, Handler: mux}
		slog.Info("readyz listening", "addr", readyzAddr)
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		return srv.ListenAndServe()
	})

	return g.Wait()
}
