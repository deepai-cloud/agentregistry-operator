#!/usr/bin/env bash
# A dedicated, explicitly selected local demo; never changes the user's kubeconfig.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
ACTION="${1:-}"
if [[ "$ACTION" != up && "$ACTION" != down ]]; then
  echo 'Usage: bash scripts/demo.sh up|down' >&2
  exit 2
fi
K3D="${K3D:-k3d}"
if [[ "$K3D" == k3d ]] && ! command -v k3d >/dev/null && [[ -x "$ROOT/.cache/tools/k3d" ]]; then
  K3D="$ROOT/.cache/tools/k3d"
fi
HELM="${HELM:-helm}"
CLUSTER=agentregistry-demo
STATE="$ROOT/.local/quickstart"
NODE="k3d-$CLUSTER-server-0"
for tool in docker "$K3D" python3; do
  command -v "$tool" >/dev/null || { echo "Required command not found: $tool" >&2; exit 1; }
done
docker version >/dev/null 2>&1 || { echo 'Docker is not reachable. Start Docker or select it with DOCKER_CONTEXT.' >&2; exit 1; }
umask 077
mkdir -p "$STATE" "$ROOT/.cache/tmp"
export KUBECONFIG="$STATE/kubeconfig"
node_id() { docker inspect --format '{{.Id}}' "$NODE" 2>/dev/null || true; }
existing="$(node_id)"
if [[ -n "$existing" ]]; then
  if [[ ! -f "$STATE/owned-node-id" ]] || [[ "$(cat "$STATE/owned-node-id")" != "$existing" ]]; then
    echo "Cluster $CLUSTER already exists and is not owned by this checkout; refusing to modify it." >&2
    exit 1
  fi
fi
if [[ "$ACTION" == down ]]; then
  if [[ -n "$existing" ]]; then
    "$K3D" cluster delete "$CLUSTER"
    rm -f "$STATE/owned-node-id" "$STATE/kubeconfig"
    echo 'Demo cluster and database deleted. Credentials retained in .local/quickstart for reuse.'
  else
    echo 'No owned demo cluster is running.'
  fi
  exit 0
fi
for tool in "$HELM" kubectl; do
  command -v "$tool" >/dev/null || { echo "Required command not found: $tool" >&2; exit 1; }
done
if [[ -z "$existing" ]]; then
  api_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
  "$K3D" cluster create "$CLUSTER" --image "${K3S_IMAGE:-rancher/k3s:v1.34.11-k3s1}" \
    --api-port "127.0.0.1:$api_port" --k3s-arg '--disable=traefik@server:0' \
    --kubeconfig-update-default=false --kubeconfig-switch-context=false \
    --wait --timeout 180s
  node_id > "$STATE/owned-node-id"
fi
"$K3D" kubeconfig get "$CLUSTER" > "$STATE/kubeconfig"
KUBE=(kubectl --kubeconfig "$STATE/kubeconfig" --context "k3d-$CLUSTER")
trap 'echo "Demo setup failed. Inspect with: kubectl --kubeconfig .local/quickstart/kubeconfig -n tenant-a get pods,areg; cleanup: make demo-down" >&2' ERR
DOCKER_BUILDKIT=1 docker build --tag agentregistry-operator:dev .
image_id="$(docker image inspect --format '{{.Id}}' agentregistry-operator:dev)"
image_tag="demo-$(printf '%s' "${image_id#sha256:}" | cut -c1-12)"
docker tag agentregistry-operator:dev "agentregistry-operator:$image_tag"
"$K3D" image import "agentregistry-operator:$image_tag" --cluster "$CLUSTER"
"$HELM" upgrade --install agentregistry-operator ./charts/agentregistry-operator \
  --kubeconfig "$STATE/kubeconfig" --kube-context "k3d-$CLUSTER" \
  --namespace agentregistry-system --create-namespace \
  --set image.repository=agentregistry-operator --set-string "image.tag=$image_tag" \
  --wait --timeout 300s
"${KUBE[@]}" apply -f examples/namespace-baseline.yaml
if [[ ! -f "$STATE/secrets.json" ]]; then
  python3 examples/local/create-secrets.py --output-dir "$STATE"
fi
"${KUBE[@]}" apply -f "$STATE/secrets.json"
"${KUBE[@]}" apply -f examples/private.yaml
"${KUBE[@]}" -n tenant-a wait --for=condition=Ready agentregistry/catalog --timeout=300s
# On reruns a previous Ready condition may precede reconciliation of the new image.
"${KUBE[@]}" -n tenant-a wait \
  --for="jsonpath={.spec.template.spec.containers[1].image}=agentregistry-operator:$image_tag" \
  deployment/catalog --timeout=300s
"${KUBE[@]}" -n tenant-a rollout status deployment/catalog --timeout=300s
"${KUBE[@]}" -n tenant-a wait --for=condition=Ready agentregistry/catalog --timeout=300s
"${KUBE[@]}" -n tenant-a get agentregistry/catalog
cat <<'MESSAGE'

Your registry is ready. Start a port-forward in another terminal:
  kubectl --kubeconfig .local/quickstart/kubeconfig --context k3d-agentregistry-demo -n tenant-a port-forward --address=127.0.0.1 service/catalog 18080:8080

Open http://127.0.0.1:18080 and use .local/quickstart/credentials.txt.
API check:
  curl --fail --config .local/quickstart/curl.conf http://127.0.0.1:18080/v0/servers

The operator manages PostgreSQL with persistent storage; pod restarts preserve data.
Deleting the entire demo cluster with make demo-down destroys its database volume.
MESSAGE
