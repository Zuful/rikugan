#!/usr/bin/env bash
# Smoke test of the Helm chart's rikugan serve, in the e2e kind cluster:
# the pod becomes Ready (its readiness probe is the API's /healthz, so
# the daemon started: lock, journal, watch, API), and helm upgrade keeps
# the generated token.
set -euo pipefail

release=smoke
ns=rikugan-smoke

# On failure, show why: the pod's events and the daemon's logs.
diagnose() {
  echo "--- pods" >&2
  kubectl -n "$ns" get pods -o wide >&2 || true
  echo "--- describe" >&2
  kubectl -n "$ns" describe pod -l app.kubernetes.io/component=serve >&2 || true
  echo "--- logs" >&2
  kubectl -n "$ns" logs -l app.kubernetes.io/component=serve --tail=50 >&2 || true
  kubectl -n "$ns" logs -l app.kubernetes.io/component=serve --tail=50 --previous >&2 || true
  echo "--- pvc" >&2
  kubectl -n "$ns" get pvc >&2 || true
}
trap 'diagnose' ERR

kubectl create namespace "$ns"

install() {
  helm upgrade --install "$release" helm/rikugan -n "$ns" \
    --set image.repository=rikugan --set image.tag=test --set image.pullPolicy=Never \
    --set prometheus.url=http://prometheus.monitoring:9090 \
    --set serve.enabled=true --wait=false
}

install
kubectl -n "$ns" rollout status "deployment/${release}-rikugan-serve" --timeout=180s

secret="${release}-rikugan-serve"
before=$(kubectl -n "$ns" get secret "$secret" -o jsonpath='{.data.token}')
install
after=$(kubectl -n "$ns" get secret "$secret" -o jsonpath='{.data.token}')
if [ -z "$before" ] || [ "$before" != "$after" ]; then
  echo "the token changed across helm upgrade" >&2
  exit 1
fi

replicas=$(kubectl -n "$ns" get deployment "${release}-rikugan-serve" -o jsonpath='{.spec.replicas}/{.spec.strategy.type}')
if [ "$replicas" != "1/Recreate" ]; then
  echo "want a single replica with the Recreate strategy, got $replicas" >&2
  exit 1
fi

logs=$(kubectl -n "$ns" logs "deployment/${release}-rikugan-serve")
if ! grep -q '"rikugan serve starting"' <<<"$logs"; then
  echo "rikugan serve did not log its start:" >&2
  echo "$logs" >&2
  exit 1
fi
echo "helm smoke test passed: serve Ready, token kept across upgrade, single instance"
