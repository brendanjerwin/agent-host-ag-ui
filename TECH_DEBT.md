# TECH_DEBT — agent-host-ag-ui

## Active

1. **ModelConfig provider deviation (user to approve/restore)**: plan locked
   `openAI.baseUrl: https://opencode.ai/zen/v1` + `mimo-v2.6-flash` with a
   1Password-synced secret. No opencode-zen credential on the devbox, so the
   day-one proof runs on OpenRouter (`https://openrouter.ai/api/v1`,
   `deepseek/deepseek-chat-v3.1`, existing `OPENROUTER_API_KEY`). Restore the
   zen endpoint + secret when the key exists. The adapter/driver/harness are
   unaffected (omp child owns every model call).
2. **kagent alpha-on-alpha**: substrate `0.3.0-alpha3` + kagent
   `1.0.0-alpha7` on the k3d testbed. Both resolve from their official OCI
   charts; identity bootstrap is manual (`kubectl-ate`, no chart performs it).
   Expect API churn; retreat ladder: pin-and-fix-forward → substrate-less
   plain pod → contract-only (documented in plan).
3. **gVisor bring-up via packaged ateom-gvisor worker image** rather than the
   documented gVisor k8s-installer DaemonSet + RuntimeClass: the substrate
   worker image bundles the sandbox path (sandboxClass `gvisor` +
   `gvisor-default` SandboxConfig). Rootless-podman k3d nodes make the
   installer DaemonSet path fragile; revisit if substrate requires host
   `runsc`.
4. **Rootless podman k3d constraints**: no docker daemon on the devbox; k3d
   runs on a podman API service at `~/.podman/podman.sock` (DOCKER_HOST +
   DOCKER_SOCK must both point there; `/var/run/docker.sock` tool-node mount
   is impossible under rootless podman). Everything disposable.
5. **No CI rung for kagent in-cluster**: CI runs vet/test/build + main-branch
   image publish only. In-cluster proofs (steps 3–7 of the verification
   ladder) are manual today; consider a nightly smoke job later.
6. **OpenDots / phase-2**: OpenDots fork work (HttpAgent per dot, SqliteAgentRunner,
   no Intelligence) is deferred to phase 2; the Driver interface here is the seam.

## Resolved

- (none yet)
