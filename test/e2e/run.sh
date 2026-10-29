#!/usr/bin/env bash
# End-to-end test on kind: cert-manager + Pebble (ACME) + the fake Hostinger
# API/DNS + this webhook installed from the Helm chart. Verifies issuance for
# a Certificate (apex + wildcard) and a Gateway, then waits for cert-manager
# to renew both on its own, and checks the TXT records were cleaned up.
#
# Usage: test/e2e/run.sh            (creates kind cluster "hostinger-e2e")
#   KEEP_CLUSTER=1  keep the cluster afterwards
#   SKIP_BUILD=1    reuse already-loaded images
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
E2E="$ROOT/test/e2e"
CLUSTER="${KIND_CLUSTER:-hostinger-e2e}"
KIND="${KIND:-kind}"
IMAGE_REPO="cert-manager-webhook-hostinger"
IMAGE_TAG="e2e"
GROUP_NAME="acme.hostinger.taha2samy-3.github.io"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-$(cd "$ROOT" && go list -m -f '{{.Version}}' github.com/cert-manager/cert-manager)}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.4.1}"
TIMEOUT="${TIMEOUT:-300s}"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

dump() {
  log "Failure diagnostics"
  kubectl get clusterissuers,certificates,certificaterequests,orders,challenges -A -o wide || true
  kubectl describe challenges -A || true
  kubectl -n cert-manager logs deploy/cert-manager --tail=100 || true
  kubectl -n cert-manager logs deploy/hostinger-webhook-cert-manager-webhook-hostinger --tail=100 || true
  kubectl -n e2e logs deploy/fake-hostinger --tail=100 || true
  kubectl -n e2e logs deploy/pebble --tail=50 || true
}

cleanup() {
  local rc=$?
  [[ $rc -ne 0 ]] && dump
  if [[ -z "${KEEP_CLUSTER:-}" ]]; then "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  exit $rc
}
trap cleanup EXIT

revision() { kubectl -n e2e get certificate "$1" -o jsonpath='{.status.revision}' 2>/dev/null || true; }

wait_for() { # wait_for <description> <timeout seconds> <command...>
  local what=$1 secs=$2; shift 2
  for ((i = 0; i < secs; i += 5)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 5
  done
  echo "timed out waiting for $what" >&2
  return 1
}

log "Creating kind cluster $CLUSTER"
"$KIND" get clusters | grep -qx "$CLUSTER" || "$KIND" create cluster --name "$CLUSTER" --wait 120s
kubectl config use-context "kind-$CLUSTER" >/dev/null

if [[ -z "${SKIP_BUILD:-}" ]]; then
  log "Building images"
  docker build -q -t "$IMAGE_REPO:$IMAGE_TAG" --build-arg VERSION=e2e "$ROOT"
  docker build -q -t fake-hostinger:e2e -f "$E2E/Dockerfile.fake" "$ROOT"
fi
"$KIND" load docker-image --name "$CLUSTER" "$IMAGE_REPO:$IMAGE_TAG" fake-hostinger:e2e

log "Installing Gateway API $GATEWAY_API_VERSION CRDs"
kubectl apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/standard-install.yaml"

log "Deploying fake Hostinger API + DNS"
kubectl apply -f "$E2E/manifests/fake-hostinger.yaml"
kubectl -n e2e rollout status deploy/fake-hostinger --timeout="$TIMEOUT"
FAKE_DNS="$(kubectl -n e2e get svc fake-hostinger -o jsonpath='{.spec.clusterIP}'):53"

log "Installing cert-manager $CERT_MANAGER_VERSION (DNS checks against $FAKE_DNS)"
helm upgrade --install cert-manager cert-manager \
  --repo https://charts.jetstack.io --version "$CERT_MANAGER_VERSION" \
  --namespace cert-manager --create-namespace --wait --timeout "$TIMEOUT" \
  --set crds.enabled=true \
  --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 \
  --set config.kind=ControllerConfiguration \
  --set config.enableGatewayAPI=true \
  --set "extraArgs[0]=--dns01-recursive-nameservers-only" \
  --set "extraArgs[1]=--dns01-recursive-nameservers=$FAKE_DNS"

log "Deploying Pebble"
sed "s|FAKE_DNS_ADDR|$FAKE_DNS|" "$E2E/manifests/pebble.yaml" | kubectl apply -f -
kubectl -n e2e rollout status deploy/pebble --timeout="$TIMEOUT"

log "Installing webhook chart"
helm upgrade --install hostinger-webhook "$ROOT/deploy/cert-manager-webhook-hostinger" \
  --namespace cert-manager --wait --timeout "$TIMEOUT" \
  --set groupName="$GROUP_NAME" \
  --set image.repository="$IMAGE_REPO" --set image.tag="$IMAGE_TAG" --set image.pullPolicy=Never \
  --set replicaCount=2 --set podDisruptionBudget.enabled=true
kubectl wait --for=condition=Available "apiservice/v1alpha1.$GROUP_NAME" --timeout="$TIMEOUT"
kubectl get --raw "/apis/$GROUP_NAME/v1alpha1" | grep -q '"name":"hostinger"'

log "Creating ClusterIssuer"
sed "s|GROUP_NAME|$GROUP_NAME|" "$E2E/manifests/issuer.yaml" | kubectl apply -f -
kubectl wait --for=condition=Ready clusterissuer/pebble --timeout="$TIMEOUT"

log "Issuing Certificate (apex + wildcard)"
kubectl apply -f "$E2E/manifests/certificate.yaml"
kubectl -n e2e wait --for=condition=Ready certificate/example-com --timeout="$TIMEOUT"

log "Issuing certificate for Gateway listener"
kubectl apply -f "$E2E/manifests/gateway.yaml"
wait_for "gateway-shim Certificate" 120 kubectl -n e2e get certificate gw-example-com-tls
kubectl -n e2e wait --for=condition=Ready certificate/gw-example-com-tls --timeout="$TIMEOUT"
kubectl -n e2e get certificate gw-example-com-tls -o jsonpath='{.spec.dnsNames}' | grep -q '\*.gw.example.com'

log "Waiting for automatic renewal (revision 1 -> 2)"
for cert in example-com gw-example-com-tls; do
  [[ "$(revision "$cert")" == 1 ]] || { echo "unexpected initial revision for $cert: $(revision "$cert")" >&2; exit 1; }
done
for cert in example-com gw-example-com-tls; do
  wait_for "$cert renewal" 300 bash -c "[[ \$(kubectl -n e2e get certificate $cert -o jsonpath='{.status.revision}') -ge 2 ]]"
  kubectl -n e2e wait --for=condition=Ready "certificate/$cert" --timeout="$TIMEOUT"
  echo "$cert renewed: revision $(revision "$cert")"
done

log "Checking challenge TXT records were cleaned up"
wait_for "challenges to be removed" 120 bash -c '[[ -z $(kubectl get challenges -A -o name) ]]'
records="$(kubectl get --raw /api/v1/namespaces/e2e/services/http:fake-hostinger:80/proxy/debug/zones/example.com)"
if grep -q '"TXT"' <<<"$records"; then
  echo "leftover TXT records: $records" >&2
  exit 1
fi

log "E2E passed"
