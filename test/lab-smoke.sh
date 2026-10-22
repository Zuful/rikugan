#!/usr/bin/env bash
# Smoke test of the local lab (deploy/lab) on a real Docker: ./lab.sh
# brings it up, Prometheus scrapes every replica of the shop with the
# labels rikugan selects on, the load reaches the services, rikugan sees
# their rollouts, and the scenarios really change what the service does.
# The verdicts themselves take 30 minutes and more: the end-to-end test
# covers the rules, this covers the lab's plumbing.
set -euo pipefail

lab=$(cd "$(dirname "$0")/../deploy/lab" && pwd)
cd "$lab"
if [ -e .env ] || [ -e state ] || [ -e export ]; then
  echo "deploy/lab already has a .env, state/ or export/: not touching it" >&2
  exit 1
fi
cp .env.example .env
cat >>.env <<'EOF'
LAB_PROMETHEUS_PORT=19090
LAB_RIKUGAN_PORT=18081
RIKUGAN_SERVE_TOKEN=lab-smoke-token
EOF
prom=http://127.0.0.1:19090

cleanup() {
  docker compose -f compose.shop.yaml down -v -t 0 >/dev/null 2>&1 || true
  docker compose -f compose.yaml down -v -t 0 >/dev/null 2>&1 || true
  rm -rf .env state export
}
diagnose() {
  echo "--- prometheus: history series, TSDB status, volume" >&2
  curl -s "$prom/api/v1/query" --data-urlencode 'query=count(http_requests_total{instance="history"})' >&2 || true
  echo >&2
  curl -s "$prom/api/v1/status/tsdb" >&2 || true
  echo >&2
  docker run --rm -v rikugan-lab_prometheus-data:/prometheus alpine:3.20 ls -la /prometheus >&2 || true
  echo "--- containers" >&2
  docker ps -a >&2 || true
  for c in rikugan-lab-serve rikugan-lab-loadgen rikugan-lab-prometheus; do
    echo "--- $c" >&2
    docker logs --tail 50 "$c" >&2 || true
  done
}
trap cleanup EXIT
trap diagnose ERR

# query prints the value of a PromQL instant query, or nothing.
query() {
  curl -fsS "$prom/api/v1/query" --data-urlencode "query=$1" 2>/dev/null |
    sed -n 's/.*"value":\[[^,]*,"\([^"]*\)"\].*/\1/p'
}

# wait_for runs its arguments until they succeed, for up to $1 seconds.
wait_for() {
  local timeout=$1 what=$2
  shift 2
  for _ in $(seq "$timeout"); do
    if "$@"; then return 0; fi
    sleep 1
  done
  echo "timed out after ${timeout}s: $what" >&2
  return 1
}

positive() { v=$(query "$1"); [ -n "$v" ] && awk -v v="$v" 'BEGIN { exit !(v > 0) }'; }
equals() { [ "$(query "$1")" = "$2" ]; }
journal_has() { docker exec rikugan-lab-serve cat /var/lib/rikugan/verdicts.jsonl 2>/dev/null | grep -c "$1" || true; }
rollouts() { journal_has "\"kind\":\"rollout\".*\"name\":\"$1\""; }
more_rollouts() { [ "$(rollouts "$1")" -gt "$2" ]; }

./lab.sh up

wait_for 120 "the five services scraped, labelled namespace and service" \
  equals 'count(up{namespace="lab-shop",service=~"web|api|low|worker|db"} == 1)' 5
wait_for 120 "traffic from the loadgen on web" \
  positive 'sum(rate(http_requests_total{namespace="lab-shop",service="web"}[1m]))'
wait_for 60 "traffic on api" \
  positive 'sum(rate(http_requests_total{namespace="lab-shop",service="api"}[1m]))'
wait_for 120 "traffic on low" \
  positive 'sum(increase(http_requests_total{namespace="lab-shop",service="low"}[2m]))'
wait_for 60 "web and api call db" \
  positive 'sum(rate(http_requests_total{namespace="lab-shop",service="db"}[1m]))'
wait_for 60 "the worker's jobs" \
  positive 'sum(worker_jobs_total{namespace="lab-shop",service="worker"})'
wait_for 30 "memory per replica (the lab's memory template)" \
  positive 'avg(process_resident_memory_bytes{namespace="lab-shop",service="web"})'
wait_for 60 "rikugan sees web's rollout" more_rollouts web 0
[ "$(rollouts api)" -ge 1 ] || { echo "rikugan does not see api's rollout" >&2; exit 1; }

# A new revision: a rollout for rikugan, 5xx for Prometheus.
before=$(rollouts web)
./lab.sh deploy web errors
wait_for 90 "rikugan records web's new rollout" more_rollouts web "$before"
wait_for 120 "web answers 5xx" \
  positive 'sum(rate(http_requests_total{namespace="lab-shop",service="web",code="500"}[1m]))'

# OOM: the kernel kills the container, and Docker says so.
./lab.sh deploy api oom
oom_killed() {
  for id in $(docker compose -f compose.shop.yaml ps -aq api); do
    [ "$(docker inspect -f '{{.State.OOMKilled}} {{.RestartCount}}' "$id")" != "false 0" ] && return 0
  done
  return 1
}
wait_for 60 "api's replica OOM-killed or restarted" oom_killed
./lab.sh deploy api healthy

./lab.sh prometheus stop
./lab.sh prometheus start
wait_for 60 "Prometheus back" equals 'count(up{namespace="lab-shop",service=~"web|api|low|worker|db"} == 1)' 5

# The shared dependency down: web answers 503.
./lab.sh dependency stop
wait_for 120 "web answers 503 without db" \
  positive 'sum(rate(http_requests_total{namespace="lab-shop",service="web",code="503"}[1m]))'
./lab.sh dependency start
./lab.sh deploy worker leak
if ./lab.sh deploy worker errors 2>/dev/null; then
  echo "an HTTP scenario was accepted for the worker" >&2
  exit 1
fi
./lab.sh phase

# A day of synthetic past, ending where the lab's own data begins.
./lab.sh history 1
wait_for 60 "Prometheus back after the backfill" equals 'count(up{namespace="lab-shop",service=~"web|api|low|worker|db"} == 1)' 5
wait_for 30 "web's traffic 12 hours ago, from the history" \
  positive 'sum(rate(http_requests_total{namespace="lab-shop",service="web"}[1m] offset 12h))'
wait_for 30 "a window p95 from the history" \
  positive 'histogram_quantile(0.95, sum by (le) (increase(http_request_duration_seconds_bucket{namespace="lab-shop",service="web"}[900s] offset 12h)))'
if ./lab.sh history 1 2>/dev/null; then
  echo "a second history was loaded over the first" >&2
  exit 1
fi

./lab.sh verdicts >/dev/null
./lab.sh export
tarball=$(ls export/*.tar.gz)
# Read the archive once: grep -q stops at the first match, and tar
# writing into the closed pipe then fails the step under pipefail.
listing=$(tar -tzf "$tarball")
for f in verdicts.json verdicts.csv actions.jsonl serve.log journal/verdicts.jsonl report.md notifications.csv; do
  grep -q "/$f\$" <<<"$listing" || { echo "export lacks $f" >&2; exit 1; }
done
grep -q '"action":"deploy","service":"api","scenario":"oom","severity":"degraded","signals":\["restarts","availability"\]' state/actions.jsonl
grep -q '"action":"dependency-stop".*"services":\["web","api"\]' state/actions.jsonl
report=$(tar -xzOf "$tarball" --wildcards '*/report.md')
grep -q '^## Criteria' <<<"$report" || { echo "no criteria in the score" >&2; exit 1; }
echo "lab smoke test passed"
