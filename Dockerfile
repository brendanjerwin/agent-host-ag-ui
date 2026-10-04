# One-unit image: adapter (PID 1) + omp child + agent-browser (Chromium via CDP).
# Build context must be the repo root.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/agent-host-ag-ui ./cmd/agent-host-ag-ui

# --- runtime: node base ships agent-browser + Chromium deps cleanly ---
FROM node:22-bookworm-slim AS runtime
# Chromium for agent-browser (CDP)
RUN apt-get update && apt-get install -y --no-install-recommends \
        chromium ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g agent-browser && agent-browser install || true

# omp coding agent (npm package)
RUN npm install -g @oh-my-pi/pi-coding-agent@18.6.0

COPY --from=build /out/agent-host-ag-ui /usr/local/bin/agent-host-ag-ui

ENV AGENT_HOST_AG_UI_ADDR=":8090" \
    AGENT_HOST_AG_UI_A2A_ADDR=":80" \
    AGENT_HOST_AG_UI_A2A_HTTP_ADDR=":8091" \
    AGENT_HOST_AG_UI_READYZ_ADDR=":8081" \
    AGENT_HOST_AG_UI_SESSION_ROOT="/data/sessions" \
    AGENT_HOST_AG_UI_CWD="/workspace"

# cap_net_bind_service for gRPC :80; non-root-friendly file ownership
RUN mkdir -p /data/sessions /workspace && chmod 0755 /data /workspace && \
    setcap cap_net_bind_service=+ep /usr/local/bin/agent-host-ag-ui || true

EXPOSE 80 8090 8091 8081
ENTRYPOINT ["/usr/local/bin/agent-host-ag-ui"]
