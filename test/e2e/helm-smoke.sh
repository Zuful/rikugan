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
  kubectl -n "$ns" describe pod -l 'app.kubernetes.io/component in (serve,opencode)' >&2 || true
  echo "--- logs" >&2
  kubectl -n "$ns" logs -l 'app.kubernetes.io/component in (serve,opencode)' --tail=50 >&2 || true
  kubectl -n "$ns" logs -l 'app.kubernetes.io/component in (serve,opencode)' --tail=50 --previous >&2 || true
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

# Escalation: OpenCode in its own Deployment, rikugan serve wired to it.
# A notification channel is required; a placeholder Telegram bot does,
# nothing is sent during the test.
helm upgrade --install "$release" helm/rikugan -n "$ns" \
  --set image.repository=rikugan --set image.tag=test --set image.pullPolicy=Never \
  --set prometheus.url=http://prometheus.monitoring:9090 \
  --set serve.enabled=true --set serve.escalation.enabled=true \
  --set 'serve.env[0].name=TELEGRAM_BOT_TOKEN' --set-string 'serve.env[0].value=0:placeholder' \
  --set 'serve.env[1].name=TELEGRAM_CHAT_ID' --set-string 'serve.env[1].value=1' --wait=false
kubectl -n "$ns" rollout status "deployment/${release}-rikugan-opencode" --timeout=300s
kubectl -n "$ns" rollout status "deployment/${release}-rikugan-serve" --timeout=180s
sa_token=$(kubectl -n "$ns" get pod -l app.kubernetes.io/component=opencode -o jsonpath='{.items[0].spec.automountServiceAccountToken}')
if [ "$sa_token" != "false" ]; then
  echo "the OpenCode pod mounts a ServiceAccount token" >&2
  exit 1
fi
logs=$(kubectl -n "$ns" logs "deployment/${release}-rikugan-serve")
if ! grep -q '"escalation":true' <<<"$logs" || ! grep -q '"agent MCP server"' <<<"$logs"; then
  echo "rikugan serve did not start with escalation:" >&2
  echo "$logs" >&2
  exit 1
fi

# The generated OpenCode config is the one the server loaded: rikugan's
# MCP server alone, and native tools rikugan knows. This is the check
# rikugan runs before each investigation.
password=$(kubectl -n "$ns" get secret "${release}-rikugan-escalation" -o go-template='{{index .data "opencode-password"}}' | base64 -d)
if [ -z "$password" ]; then
  echo "no OpenCode password in the escalation Secret" >&2
  exit 1
fi
kubectl -n "$ns" port-forward "svc/${release}-rikugan-opencode" 14096:4096 >/dev/null 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
# get <path>: OpenCode's answer with the password, the status on
# stderr and a failure past 60 s.
get() {
  local code
  for _ in $(seq 1 30); do
    code=$(curl -s -o "$work_dir/body" -w '%{http_code}' -u "opencode:$password" "http://127.0.0.1:14096$1" || true)
    if [ "$code" = 200 ]; then
      cat "$work_dir/body"
      return 0
    fi
    sleep 2
  done
  echo "GET $1: HTTP $code: $(head -c 500 "$work_dir/body" 2>/dev/null)" >&2
  echo "--- opencode logs" >&2
  kubectl -n "$ns" logs "deployment/${release}-rikugan-opencode" --tail=120 >&2 || true
  return 1
}
work_dir=$(mktemp -d)
get /config >/dev/null
status=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:14096/config)
if [ "$status" != "401" ]; then
  echo "OpenCode answered $status without its password, want 401" >&2
  exit 1
fi
mcp=$(get /config | jq -c '.mcp | keys')
if [ "$mcp" != '["rikugan"]' ]; then
  echo "OpenCode's MCP servers are $mcp, want [\"rikugan\"]" >&2
  exit 1
fi
ids=$(get /experimental/tool/ids)
unknown=$(jq -r --argjson known '["invalid","question","bash","read","glob","grep","edit","write","task","webfetch","todowrite","todoread","websearch","skill","apply_patch","list","patch","multiedit","lsp","codesearch"]' \
  '[.[] | select(startswith("rikugan_") | not) | select(. as $t | $known | index($t) | not)] | join(",")' <<<"$ids")
if [ -n "$unknown" ]; then
  echo "OpenCode lists native tools rikugan does not know: $unknown" >&2
  exit 1
fi
echo "helm smoke test passed: serve Ready, token kept across upgrade, single instance; OpenCode Ready, password required, config loaded, natives known"
