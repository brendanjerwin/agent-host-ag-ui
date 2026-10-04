# deploy/

Day-one testbed + kagent wiring for `agent-host-ag-ui`.

## Layout

- `kagent-values.yaml` — helm values for `kagent` 1.0.0-alpha7 with the Agent
  Substrate integration enabled (`controller.substrate.*`).
- `manifests.yaml` — ModelConfig / AgentTemplate / Harness / Agent CRs.

## Testbed bring-up (disposable k3d micro-cluster; never touches production)

```bash
# Podman service (home-dir socket; see TECH_DEBT.md for why)
podman system service unix://$HOME/.podman/podman.sock --time=0 &
export DOCKER_HOST=unix://$HOME/.podman/podman.sock
export DOCKER_SOCK=$DOCKER_HOST

# Pinned k3s 1.37 with the pod-certificates runtime config
k3d cluster create agent-host --image rancher/k3s:v1.37.0-k3s1 \
  --k3s-arg "--kube-apiserver-arg=runtime-config=certificates.k8s.io/v1beta1=true@server:0" \
  --agents 1
k3d kubeconfig write agent-host --output $HOME/.k3d-agent-host-kubeconfig --overwrite
export KUBECONFIG=$HOME/.k3d-agent-host-kubeconfig

# Agent Substrate 0.3.0-alpha3 (crds -> platform -> kubectl-ate identity
# bootstrap -> re-upgrade with --wait), then kagent 1.0.0-alpha7
helm upgrade --install substrate-crds oci://ghcr.io/kagent-dev/substrate/helm/substrate-crds \
  --version 0.3.0-alpha3 --namespace ate-system --create-namespace --wait
helm upgrade --install substrate oci://ghcr.io/kagent-dev/substrate/helm/substrate \
  --version 0.3.0-alpha3 --namespace ate-system
# ... kubectl-ate admin make-ca-pool / make-jwt-pool steps (kagent.dev 1.x docs) ...
helm upgrade substrate oci://ghcr.io/kagent-dev/substrate/helm/substrate \
  --version 0.3.0-alpha3 --namespace ate-system --reuse-values --wait --timeout 10m
helm upgrade --install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
  --version 1.0.0-alpha7 --namespace kagent --create-namespace --timeout 10m \
  -f deploy/kagent/kagent-values.yaml
```

## Promotion ladder

1. **kagent byo Harness pod**: `kubectl apply -f deploy/kagent/manifests.yaml`
   (with the image SHA set); pod `Running`, `/readyz` 200.
2. **A2A session smoke**: kagent UI or agentgateway fronting the pod's :80 —
   send a message, stream updates, verify the omp child answered.
3. **AG-UI face smoke**: `go run ./cmd/agui-check --url http://<pod>:8090/ag-ui`
   exit 0 + browser test GUI at `/`.
4. **Production adoption**: after the production cluster's >=1.37 upgrade (in
   flight; delegated workstream), deploy the image + CRs there via ArgoCD.
