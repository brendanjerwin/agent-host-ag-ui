#!/usr/bin/env bash
# Testbed bring-up for agent-host-ag-ui + kagent + E2B Embed — the GitOps-way
# reproducible sequence. Everything here is committed config or pinned images;
# no hand-edited cluster state beyond this script.
#
# Prereqs (devbox): k3d, kubectl, helm, jq, openssl, git-lfs, podman.
# Root (sudo) is needed once for: cgroup delegation, hugepages, nbd module,
# /dev/nbd* + /dev/net/tun nodes, ext4 image mount.
#
# Usage: ./deploy/bringup.sh

set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export DOCKER_HOST="unix:///home/brendanjerwin/.podman/podman.sock"
export DOCKER_SOCK="$DOCKER_HOST"
export KUBECONFIG=/home/brendanjerwin/.k3d-agent-host-kubeconfig
export PATH="/home/brendanjerwin/src/agent-host-ag-ui/.devbox/nix/profile/default/bin:/home/brendanjerwin/.local/bin:$PATH"
HELM=/home/brendanjerwin/.local/share/devbox/global/default/.devbox/nix/profile/default/bin/helm

SVC() { systemctl --user "$@"; }

echo "== 1. podman API service"
pgrep -f 'podman system service unix:///home/brendanjerwin/.podman/podman.sock' >/dev/null ||
 (podman system service unix:///home/brendanjerwin/.podman/podman.sock --time=0 &)

echo "== 2. host prerequisites (idempotent)"
sudo -n sysctl -w vm.nr_hugepages=4096 >/dev/null
sudo -n modprobe nbd nbds_max=64 2>/dev/null || true
sudo -n sh -c '[ -b /dev/nbd0 ] || for i in $(seq 0 63); do mknod /dev/nbd$i b 43 $i 2>/dev/null; done'
sudo -n sh -c '[ -c /dev/net/tun ] || mknod /dev/net/tun c 10 200'
sudo -n sh -c 'mkdir -p /etc/modules-load.d; echo nbd > /etc/modules-load.d/e2b.conf'
echo "vm.nr_hugepages=4096" | sudo -n tee /etc/sysctl.d/90-e2b.conf >/dev/null

echo "== 3. k3d cluster (nodes bind the ext4 store for substrate actors)"
mkdir -p /home/brendanjerwin/.k3d-ate-store
if ! k3d cluster list 2>/dev/null | grep -q '^agent-host '; then
 k3d cluster create agent-host \
  --image rancher/k3s:v1.37.0-k3s1 \
  --k3s-arg "--kube-apiserver-arg=runtime-config=certificates.k8s.io/v1beta1=true@server:0" \
  --agents 1 \
  -v /home/brendanjerwin/.k3d-ate-store:/var/lib/ate@server:0 \
  -v /home/brendanjerwin/.k3d-ate-store:/var/lib/ate@agent:0
fi
k3d kubeconfig write agent-host --output "$KUBECONFIG" --overwrite >/dev/null
sleep 20
kubectl get nodes

echo "== 4. node images (from GHCR or local builds; ctr import avoids node pulls)"
for node in k3d-agent-host-server-0 k3d-agent-host-agent-0; do
 for img in "${AGENT_HOST_IMAGE_TAR:-/home/brendanjerwin/.tmp-agui-dev8.tar}" "${KAGENT_UI_IMAGE_TAR:-/home/brendanjerwin/.tmp-kagentui.tar}"; do
  [ -f "$img" ] || continue
  podman cp "$img" "$node:/tmp/import.tar"
  podman exec "$node" sh -c 'ctr -a /run/k3s/containerd/containerd.sock -n k8s.io images import /tmp/import.tar' >/dev/null
 done
done

echo "== 5. Agent Substrate (crds -> substrate -> kubectl-ate identity -> re-upgrade)"
$HELM upgrade --install substrate-crds oci://ghcr.io/kagent-dev/substrate/helm/substrate-crds \
 --version 0.3.0-alpha3 --namespace ate-system --create-namespace --wait
$HELM upgrade --install substrate oci://ghcr.io/kagent-dev/substrate/helm/substrate \
 --version 0.3.0-alpha3 --namespace ate-system -f - <<'EOF'
credentialProvider:
  namespacePolicies:
  - atespace: kagent
    allowedNamespaces: [kagent]
EOF

kubectl ate admin make-ca-pool --ca-id=1 --name=service-dns-ca-pool --secret-namespace=podcertificate-controller-system
kubectl ate admin make-ca-pool --ca-id=1 --name=pod-identity-ca-pool --secret-namespace=podcertificate-controller-system
kubectl ate admin make-jwt-pool --key-id=1 --name=actor-id-jwt-pool --secret-namespace=ate-system
kubectl ate admin make-ca-pool --ca-id=1 --name=actor-id-ca-pool --secret-namespace=ate-system
kubectl ate admin make-ca-pool --ca-id=1 --name=egress-mitm-ca-pool --secret-namespace=ate-system --key-type=ECDSAP256

actor_id_ca_root="$(kubectl get secret actor-id-ca-pool -n ate-system -o jsonpath='{.data.pool}' | base64 --decode | jq -r '.CAs[0].RootCertificateDER' | base64 --decode | openssl x509 -inform der -outform pem)"
kubectl create secret generic actor-id-ca-certs -n ate-system --from-literal=ca.crt="${actor_id_ca_root}" --dry-run=client -o yaml | kubectl apply -f -
k8s_issuer="$(kubectl get --raw /.well-known/openid-configuration | jq -r .issuer)"
kubectl create configmap ate-api-authentication -n ate-system --from-literal="authentication.yaml=actorIdentityJWTProvider: kubernetes
jwtProviders:
- name: kubernetes
  issuer: ${k8s_issuer}
  audiences: [api.ate-system.svc]
  certificateAuthorityFile: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  discoveryTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
" --dry-run=client -o yaml | kubectl apply -f -

$HELM upgrade substrate oci://ghcr.io/kagent-dev/substrate/helm/substrate \
 --version 0.3.0-alpha3 --namespace ate-system --reuse-values --wait --timeout 10m

echo "== 6. kagent"
$HELM upgrade --install kagent-crds oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
 --version 1.0.0-alpha7 --namespace kagent --create-namespace --wait
$HELM upgrade --install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
 --version 1.0.0-alpha7 --namespace kagent --create-namespace --timeout 10m \
 -f "$REPO/deploy/kagent/kagent-values.yaml"
kubectl -n kagent rollout status deploy/kagent-controller --timeout=300s

echo "== 7. agent-host CRs + rung-3 deployment + secrets"
python3 - <<'PYEOF'
import sqlite3, json, subprocess, os
db = sqlite3.connect('/home/brendanjerwin/.omp/agent/agent.db')
keys = {}
for prov in ('ollama-cloud', 'opencode-go'):
    keys[prov] = json.loads(db.execute("SELECT data FROM auth_credentials WHERE provider=?", (prov,)).fetchone()[0])['key']
secrets = {
    'OLLAMA_CLOUD_API_KEY': keys['ollama-cloud'],
    'OPENCODE_API_KEY': keys['opencode-go'],
    'OPENROUTER_API_KEY': os.environ.get('OPENROUTER_API_KEY', 'none'),
}
def kubectl(args, inp=None):
    r = subprocess.run(['kubectl','-n','kagent']+args, input=inp, capture_output=True, text=True)
    print((r.stdout or r.stderr).strip())
kubectl(['create','secret','generic','omp-key','--dry-run=client','-o','yaml',
    '--from-literal=OLLAMA_CLOUD_API_KEY='+secrets['OLLAMA_CLOUD_API_KEY'],
    '--from-literal=OPENCODE_API_KEY='+secrets['OPENCODE_API_KEY'],
    '--from-literal=OPENROUTER_API_KEY='+secrets['OPENROUTER_API_KEY']])
kubectl(['create','secret','generic','omp-provider-keys','--dry-run=client','-o','yaml',
    '--from-literal=OPENCODE_API_KEY='+secrets['OPENCODE_API_KEY'],
    '--from-literal=OLLAMA_CLOUD_API_KEY='+secrets['OLLAMA_CLOUD_API_KEY']])
kubectl(['create','secret','generic','omp-auth','--from-file=agent.db=/home/brendanjerwin/.omp-agent-podauth/agent.db',
    '--from-file=config.yml=/home/brendanjerwin/.omp/agent/config.yml'])
PYEOF
kubectl -n kagent apply -f "$REPO/deploy/kagent/manifests.yaml"
kubectl -n kagent apply -f "$REPO/deploy/testbed-deployment.yaml"
kubectl -n kagent set image deploy/agent-host-ag-ui host="${AGENT_HOST_IMAGE:-ghcr.io/brendanjerwin/agent-host-ag-ui:dev8}"
kubectl -n kagent set image deploy/kagent-ui ui="${KAGENT_UI_IMAGE:-ghcr.io/brendanjerwin/kagent-ui:gitops-1}"
kubectl -n kagent rollout status deploy/agent-host-ag-ui --timeout=300s

echo "== 8. E2B Embed (kubernetes path; node labelled + hugepages prereqs done above)"
kubectl label node k3d-agent-host-server-0 e2b.dev/single-node=true --overwrite
kubectl create namespace e2b --dry-run=client -o yaml | kubectl apply -f -
kubectl -n e2b create secret generic e2b-api --from-literal=ADMIN_TOKEN="$(openssl rand -hex 32)" --from-literal=SANDBOX_ACCESS_TOKEN_HASH_SEED="$(openssl rand -hex 32)" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k "https://github.com/e2b-dev/runtime//embed/kubernetes?ref=main"
kubectl -n e2b rollout status statefulset/e2b --timeout=15m || true

echo "== 9. local services + tailnet"
python3 - <<'PYEOF'
import os, subprocess
os.makedirs('/home/brendanjerwin/.config/systemd/user', exist_ok=True)
units = {
 'agenthost-pf.service': '/home/brendanjerwin/.k3d-agent-host-kubeconfig -n kagent port-forward svc/agent-host-ag-ui 18090:8090 18081:8081 18091:8091',
 'kagent-ui-pf.service': '/home/brendanjerwin/.k3d-agent-host-kubeconfig -n kagent port-forward svc/kagent-ui 18095:8080',
 'dojo.service': None,
}
PF_BIN='/home/brendanjerwin/src/agent-host-ag-ui/.devbox/nix/profile/default/bin/kubectl'
def write(path, body): open(path,'w').write(body)
write('/home/brendanjerwin/.config/systemd/user/agenthost-pf.service', f'''[Unit]
Description=agent-host-ag-ui k8s port-forward (k3d testbed)
After=network-online.target

[Service]
ExecStart={PF_BIN} --kubeconfig=/home/brendanjerwin/.k3d-agent-host-kubeconfig -n kagent port-forward svc/agent-host-ag-ui 18090:8090 18081:8081 18091:8091
Restart=on-failure
RestartSec=5
''')
write('/home/brendanjerwin/.config/systemd/user/kagent-ui-pf.service', f'''[Unit]
Description=kagent UI port-forward (k3d testbed)
After=network-online.target

[Service]
ExecStart={PF_BIN} --kubeconfig=/home/brendanjerwin/.k3d-agent-host-kubeconfig -n kagent port-forward svc/kagent-ui 18095:8080
Restart=on-failure
RestartSec=5
''')
write('/home/brendanjerwin/.config/systemd/user/dojo.service', '''[Unit]
Description=AG-UI Dojo (demo-viewer) — test client for agent-host-ag-ui
After=network-online.target

[Service]
Environment=PATH=/home/brendanjerwin/.npm-global/bin:/home/brendanjerwin/.local/share/devbox/global/default/.devbox/nix/profile/default/bin:/home/brendanjerwin/src/agent-host-ag-ui/.devbox/nix/profile/default/bin:/usr/bin:/bin
WorkingDirectory=/home/brendanjerwin/src/ag-ui/apps/dojo
ExecStart=/home/brendanjerwin/.local/share/devbox/global/default/.devbox/nix/profile/default/bin/node node_modules/next/dist/bin/next start
Restart=on-failure
RestartSec=5
''')
for u in units:
    subprocess.run(['systemctl','--user','daemon-reload'], capture_output=True)
    subprocess.run(['systemctl','--user','enable','--now',u], capture_output=True)
print('units up')
PYEOF
tailscale serve --bg 3000 >/dev/null 2>&1
tailscale serve --bg --https=8443 18090 >/dev/null 2>&1
tailscale serve --bg --https=9443 18095 >/dev/null 2>&1

echo "== bring-up complete"
kubectl get po -n kagent,ate-system,e2b 2>&1 | tail -20
