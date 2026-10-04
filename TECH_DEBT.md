# TECH_DEBT — agent-host-ag-ui

## Active

1. **kagent byo-Harness substrate actor: overlayfs-on-overlayfs (rung 3 held)**:
   two blockers surfaced, both infrastructural —
   - atelet pulls the Actor image from the registry (`go-containerregistry`),
     so node-local `ctr images import` doesn't help. Solved: CI publishes
     `ghcr.io/brendanjerwin/agent-host-ag-ui:<sha>`/`:latest` and the GHCR
     package is anonymously pullable (verified via `podman pull
     @sha256:ad3d5a47…`); the Harness now pins the CI digest.
   - **the actual crash**: golden-actor pause rootfs
     `/var/lib/ate/actors/<uid>/bundles/_pause` needs an overlayfs upperdir,
     but on rootless-podman k3d nodes that path is on the container's own
     overlay writable layer → kernel rejects nested overlay
     ("filesystem ... not supported as upperdir"). Not fixable by config on
     this testbed; needs real node storage (docker-based k3d, a VM host, or
     a dedicated bind-mount disk for /var/lib/ate).
   **Fallback ladder rung 3 exercised**: `deploy/testbed-deployment.yaml`
   runs the same one-unit image as a plain Deployment + Service, serving the
   identical byo contract (AG-UI :8090 / A2A :80/:8091 / readyz :8081) —
   adapter, driver, translator, A2A shim, and image are ALL unaffected, and
   every in-cluster proof (AG-UI streaming, A2A task completion, browser
   tool, agui-check ValidateSequence) passed on this surface. Restore the
   substrate path on a host whose /var/lib/ate can hold real overlay upper
   dirs.
2. **ModelConfig provider deviation (user to approve/restore)**: plan locked
   `openAI.baseUrl: https://opencode.ai/zen/v1` + `mimo-v2.6-flash` with a
   1Password-synced secret. No opencode-zen credential on the devbox, so the
   day-one proof runs on OpenRouter (`https://openrouter.ai/api/v1`,
   `deepseek/deepseek-chat-v3.1`, existing `OPENROUTER_API_KEY`) — declared
   in `deploy/kagent/manifests.yaml`. omp default role is `ollama-cloud`
   with the devbox key.
3. **kagent alpha-on-alpha**: substrate `0.3.0-alpha3` + kagent
   `1.0.0-alpha7`. Identity bootstrap is manual (`kubectl-ate`; no Helm
   chart creates the CA pools). Expect API churn; retreat ladder documented
   in the plan (pin-and-fix-forward → substrate-less plain pod →
   contract-only).
4. **Rootless podman k3d constraints**: no docker daemon on the devbox; k3d
   runs on a podman API service at `~/.podman/podman.sock` (DOCKER_HOST +
   DOCKER_SOCK must both point there; `/var/run/docker.sock` tool-node
   mounts impossible under rootless podman). `/etc` is effectively
   read-only; the user@.service Delegate drop-in (cpuset) was needed. All
   disposable.
5. **No CI rung for kagent in-cluster**: CI runs vet/test/build + GHCR
   publish only. The in-cluster proofs (kubectl-ate bootstrap, rung-3
   deployment) are manual today; consider a nightly smoke job later.
6. **Image size**: `agent-host-ag-ui` ~3.7GiB — node:22-bookworm-slim +
   chromium + omp native binary + agent-browser. A distroless/alpine runtime
   plus agent-browser rewrite would cut it; day-one ship used node for CDP
   chrome convenience.
7. **OpenDots / phase-2**: OpenDots fork work (HttpAgent per dot,
   SqliteAgentRunner, no Intelligence) is deferred to phase 2; the Driver
   interface here is the seam.

## Resolved
- **GUI never loaded (wire-only proof claimed as browser proof)** → loaded
  `localhost:18090/` with agent-browser and drove a full turn: text streamed
  into bubbles, tool blocks rendered, browser panel rendered the real 70KB
  screenshot (`img.src` data URI), RUN_FINISHED reached. GUI is now
  self-contained (fetch + manual SSE parse; no esm.sh CDN dependency).

- **rpm omp wrapper bun missing in-pod** → Dockerfile switched to the
  prebuilt native omp binary (`omp.sh/install --binary`); verified
  `omp --version` in-container.
- **`prompt_result{status:"error"}` suppressed after error-tail agent_end** →
  translator now emits RUN_ERROR on an error-stopReason tail and lets the
  authoritative prompt_result error override (translate.go).
- **toolCallId rename mismatch (START/ARGS placeholder vs END/RESULT real id)**
  → translator keeps a stable AG-UI-facing id (`tc-idx-*` placeholder) and
  maps omp ids through `ompToAGUI` for result pairing.
- **Stuck "in-flight" thread after client abort** → run context now derives
  from the request context so client aborts tear the omp run down (omp.go).
- **oLama A2A `gob: encodeArray: nil element`** → a2a executor now passes
  the trigger message into `a2a.NewSubmittedTask`.
- **`ModelConfig`/`Harness` manifest shape** → corrected against live CRD
  schemas (spec-level `model`/`apiKeySecret`+`apiKeySecretKey`, byo
  `workload.command` required, digest-pinned image).
