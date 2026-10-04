# TECH_DEBT — agent-host-ag-ui

## Active

1. **kagent byo-Harness substrate actor blocked on GHCR visibility (rung 3 held)**:
   the golden actor's atelet pulls the Harness image from the *registry*
   (`go-containerregistry remote.Image`), not node containerd; node-local
   `ctr images import` does not help. The session `gh` token lacks
   `write:packages`, so `brendanjerwin/agent-host-ag-ui` stays private
   (urgent flip would be a GHCR visibility change). CI still publishes to
   `ghcr.io/brendanjerwin/agent-host-ag-ui:<sha>`/`:latest` on main pushes
   (packages permissions present in CI). **Fallback ladder rung 3 exercised**:
   `deploy/testbed-deployment.yaml` runs the same one-unit image (digest-pinned,
   node-local) as a plain Deployment + Service, serving the identical byo
   contract (AG-UI :8090 / A2A :80 / :8091 / readyz :8081) without substrate
   actors — adapter, driver, translator, A2A shim, and image are ALL
   unaffected. Restore the byo path either by (a) re-auth with
   `gh auth login -s write:packages` then `gh api -X PATCH
   /user/packages/container/agent-host-ag-ui` to make it public, or (b) a
   registry the atelet can reach (in-cluster registry served over
   `127.0.0.1`).
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
