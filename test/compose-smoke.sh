#!/usr/bin/env bash
# Smoke test of deploy/compose: rikugan serve, built from the
# serve-compose image target, watches another Compose project through the
# Docker socket. It must start (healthcheck on /healthz), see the watched
# project, require its token, and refuse a second instance.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
file="$root/deploy/compose/compose.yaml"
proxy="$root/deploy/compose/compose.socket-proxy.yaml"
export WATCHED_PROJECT=rikugan-smoke-demo
export PROMETHEUS_URL=http://127.0.0.1:9   # nothing listens: checks fail, start-up must not
export RIKUGAN_SERVE_TOKEN=smoke-token
export RIKUGAN_SERVE_PORT=18080
api=http://127.0.0.1:$RIKUGAN_SERVE_PORT

cat >"$work/demo.yaml" <<'YAML'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "exec sleep 3600"]
YAML

cleanup() {
  docker compose -f "$file" -f "$proxy" down -v -t 0 >/dev/null 2>&1 || true
  docker compose -p "$WATCHED_PROJECT" -f "$work/demo.yaml" down -t 0 >/dev/null 2>&1 || true
  rm -rf "$work"
}
diagnose() {
  echo "--- rikugan-serve logs" >&2
  docker logs rikugan-serve >&2 || true
  docker logs rikugan-socket-proxy >&2 || true
  docker ps -a >&2 || true
}
trap cleanup EXIT
trap diagnose ERR

docker build --target serve-compose -t rikugan:serve-compose "$root"
docker compose -p "$WATCHED_PROJECT" -f "$work/demo.yaml" up -d
docker compose -f "$file" up -d

for _ in $(seq 60); do
  if curl -fsS "$api/healthz" >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS "$api/healthz" >/dev/null

code=$(curl -s -o /dev/null -w '%{http_code}' "$api/v1/verdicts")
[ "$code" = 401 ] || { echo "without a token: HTTP $code, want 401" >&2; exit 1; }
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $RIKUGAN_SERVE_TOKEN" "$api/v1/verdicts")
[ "$code" = 200 ] || { echo "with the token: HTTP $code, want 200" >&2; exit 1; }

logs=$(docker logs rikugan-serve 2>&1)
grep -q '"rikugan serve starting"' <<<"$logs" || { echo "no start line" >&2; exit 1; }
grep -q "\"namespaces\":\"$WATCHED_PROJECT\"" <<<"$logs" || { echo "not watching $WATCHED_PROJECT" >&2; exit 1; }

# The daemon reads the watched project: its rollout is in the journal.
sees_demo() {
  for _ in $(seq 60); do
    if docker exec rikugan-serve cat /var/lib/rikugan/verdicts.jsonl 2>/dev/null | grep -q '"kind":"rollout".*"name":"web"'; then
      return 0
    fi
    sleep 1
  done
  echo "$1: no rollout of $WATCHED_PROJECT/web in the journal" >&2
  docker exec rikugan-serve cat /var/lib/rikugan/verdicts.jsonl >&2 || true
  return 1
}
sees_demo "direct socket"

# rikugan doctor, inside the daemon's container: the watched project is
# seen, Prometheus (nothing listens) is not, so it fails.
if doctor=$(docker exec rikugan-serve rikugan doctor); then
  echo "rikugan doctor passed with no metrics backend:" >&2
  echo "$doctor" >&2
  exit 1
fi
grep -q '^✗ metrics backend' <<<"$doctor" || { echo "doctor: no metrics failure:" >&2; echo "$doctor" >&2; exit 1; }
grep -Eq "^✓ orchestration +dockercompose, $WATCHED_PROJECT: 1 services" <<<"$doctor" || { echo "doctor: watched project not seen:" >&2; echo "$doctor" >&2; exit 1; }
grep -Eq "^✓ $WATCHED_PROJECT/web +revision [0-9a-f]{12}, complete" <<<"$doctor" || { echo "doctor: web's rollout not read:" >&2; echo "$doctor" >&2; exit 1; }

if docker compose -f "$file" up -d --scale rikugan-serve=2 >/dev/null 2>&1; then
  echo "a second instance started: container_name must prevent it" >&2
  exit 1
fi

# Behind the read-only socket proxy: same, without the socket mounted.
docker compose -f "$file" down -v -t 0
docker compose -f "$file" -f "$proxy" up -d
for _ in $(seq 60); do
  if curl -fsS "$api/healthz" >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS "$api/healthz" >/dev/null
if docker inspect rikugan-serve --format '{{range .Mounts}}{{.Source}} {{end}}' | grep -q docker.sock; then
  echo "the socket is still mounted into the daemon" >&2
  exit 1
fi
sees_demo "socket proxy"
if docker exec rikugan-serve docker run --rm busybox:1.36 true >/dev/null 2>&1; then
  echo "the proxy let a write through" >&2
  exit 1
fi

echo "compose smoke test passed: serve healthy, watching $WATCHED_PROJECT, token required, single instance, read-only socket proxy"
