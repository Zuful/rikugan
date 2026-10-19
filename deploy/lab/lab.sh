#!/usr/bin/env bash
# Drives the local lab: see README.md, or ./lab.sh help.
# Written for the bash 3.2 macOS ships.
set -euo pipefail

cd "$(dirname "$0")"
LAB="docker compose -f compose.yaml"
SHOP="docker compose -f compose.shop.yaml"
JQ_IMAGE=ghcr.io/jqlang/jq:1.7.1
SERVICES="web api"

die() { echo "lab: $*" >&2; exit 1; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# record appends an action to state/actions.jsonl, with what rikugan is
# expected to report: the export compares it with the verdicts.
record() { # action service scenario expected
  mkdir -p state
  printf '{"time":"%s","action":"%s","service":"%s","scenario":"%s","expected":"%s"}\n' \
    "$(now)" "$1" "$2" "$3" "$4" >>state/actions.jsonl
  echo "[$(now)] $1 $2 $3"
  [ -z "$4" ] || echo "  expected: $4"
}

# set_env sets KEY=value in .env.
set_env() {
  grep -v "^$1=" .env >.env.tmp || true
  echo "$1=$2" >>.env.tmp
  mv .env.tmp .env
}

env_value() { grep "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- || true; }

token() { env_value RIKUGAN_SERVE_TOKEN; }

api() { # path
  port=$(env_value LAB_RIKUGAN_PORT)
  curl -fsS -H "Authorization: Bearer $(token)" "http://127.0.0.1:${port:-8080}${1}" ||
    die "rikugan's API does not answer: is the lab up? (./lab.sh status)"
}

prom_port() { port=$(env_value LAB_PROMETHEUS_PORT); echo "${port:-9090}"; }

jqc() { docker run --rm -i "$JQ_IMAGE" "$@"; }

check_service() {
  case " $SERVICES " in *" $1 "*) ;; *) die "unknown service '$1': want web or api" ;; esac
}

# scenario prints the environment of a scenario, then a line starting
# with "expected:".
scenario() {
  case "$1" in
  healthy) echo "expected: ok at +5, +15 and +30 (investigate at most during warm-up); no message" ;;
  errors)
    echo "ERROR_RATE=0.3"
    echo "expected: degraded (error_rate) from +5; one critical, no repeat at +15/+30"
    ;;
  minor-errors)
    echo "ERROR_RATE=0.02"
    echo "expected: investigate (error_rate, 2 % against 0 %); not degraded"
    ;;
  slow)
    echo "LATENCY_MS=300"
    echo "expected: latency investigate at +5 (capped), degraded from +15; one critical"
    ;;
  slightly-slow)
    echo "LATENCY_MS=60"
    echo "expected: latency investigate (about +60 ms), never degraded"
    ;;
  crash)
    echo "CRASH=true"
    echo "expected: rollout failed at the 5 min deadline, restarts degraded; one critical. Fix with: deploy <service> healthy"
    ;;
  oom)
    echo "OOM=true"
    echo "expected: rollout failed, restarts degraded with OOM kills; one critical"
    ;;
  leak)
    echo "LEAK_MB_PER_MIN=1"
    echo "expected: rollout ok; after about 6 h, a sweep reports memory_leak investigate"
    ;;
  *) return 1 ;;
  esac
}

write_env() { # service scenario
  {
    echo "# written by lab.sh: scenario $2"
    echo "LAB_SCENARIO=$2"
    echo "LAB_DEPLOYED_AT=$(date +%s)"
    scenario "$2" | grep -v '^expected:' || true
  } >"state/$1.env"
}

cmd_up() {
  docker info >/dev/null 2>&1 || die "Docker is not running: start Docker Desktop"
  mkdir -p state
  if [ ! -f .env ]; then
    cp .env.example .env
    set_env RIKUGAN_SERVE_TOKEN "$(openssl rand -hex 24)"
    echo "created .env with a fresh API token"
  fi
  if [ -z "$(env_value TELEGRAM_BOT_TOKEN)" ] && [ -z "$(env_value SLACK_WEBHOOK_URL)" ]; then
    echo "note: no notification channel in .env, verdicts are recorded but not sent"
  fi
  for s in $SERVICES; do [ -f "state/$s.env" ] || write_env "$s" healthy; done
  docker image inspect rikugan-lab-app:local rikugan:serve-compose >/dev/null 2>&1 || cmd_build
  $LAB up -d
  $SHOP up -d
  record up "" "" ""
  cat <<EOF

The lab is up.
  Prometheus  http://127.0.0.1:$(prom_port)
  Verdicts    ./lab.sh verdicts

Wait 30 minutes before the first deploy: rikugan compares each rollout
with the 30 minutes before it, and Prometheus starts empty.
EOF
}

# cmd_build builds the app and rikugan images; up builds them only
# when they are missing.
cmd_build() { $LAB build; }

cmd_deploy() { # service scenario
  [ $# -eq 2 ] || die "usage: ./lab.sh deploy <web|api> <scenario>  (./lab.sh scenarios lists them)"
  check_service "$1"
  scenario "$2" >/dev/null || die "unknown scenario '$2': ./lab.sh scenarios lists them"
  write_env "$1" "$2"
  $SHOP up -d "$1"
  record deploy "$1" "$2" "$(scenario "$2" | sed -n 's/^expected: //p')"
}

cmd_chain() { # service count minutes
  [ $# -eq 3 ] || die "usage: ./lab.sh chain <web|api> <count> <minutes between deploys>"
  check_service "$1"
  record chain "$1" "$2 deploys every $3 min" "rollouts superseded before their checkpoints are recorded superseded; past 2 h of chaining, latency and errors are not compared (no credible baseline)"
  i=1
  while [ "$i" -le "$2" ]; do
    write_env "$1" healthy
    $SHOP up -d "$1"
    echo "[$(now)] chain $1: deploy $i/$2"
    [ "$i" -eq "$2" ] || sleep $(($3 * 60))
    i=$((i + 1))
  done
}

cmd_traffic() { # service rps
  [ $# -eq 2 ] || die "usage: ./lab.sh traffic <web|api> <requests per second>"
  check_service "$1"
  key=LAB_WEB_RPS
  [ "$1" = api ] && key=LAB_API_RPS
  set_env "$key" "$2"
  $LAB up -d loadgen
  record traffic "$1" "$2 rps" "below about 0.7 rps, a 5-minute window has under 200 requests: latency and errors become insufficient data, and only the blunt rule (5xx above 20 %) can flag a rollout"
}

cmd_prometheus() { # stop|start
  case "${1:-}" in
  stop)
    $LAB stop prometheus
    record prometheus-stop "" "" "checks wait; past 2 h after a completion they are given up, with one 'rikugan cannot evaluate' warning; a crash is still notified (partial check)"
    ;;
  start)
    $LAB start prometheus
    record prometheus-start "" "" "an 'rikugan evaluates again' message if a warning was sent"
    ;;
  *) die "usage: ./lab.sh prometheus <stop|start>" ;;
  esac
}

cmd_verdicts() { # [since]
  api "/v1/verdicts?since=${1:-24h}&limit=1000" | jqc -r '
    .verdicts | reverse | .[] |
    [ .time[0:19], .service, .source, (.revision // ""), (.checkpoint // ""),
      (.verdict // .outcome) + (if .partial then " (metrics down)" else "" end),
      ([.signals[]? | select(.status != "ok" and .status != "not_evaluated") | "\(.name)=\(.status)"] | join(" ")) ]
    | @tsv'
}

cmd_status() {
  $LAB ps
  $SHOP ps
  echo
  for s in $SERVICES; do echo "$s: scenario $(grep '^LAB_SCENARIO=' "state/$s.env" | cut -d= -f2)"; done
  echo
  echo "Last verdicts:"
  cmd_verdicts 6h | tail -10
}

cmd_export() {
  dir="export/$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$dir"
  api "/v1/verdicts?since=720h&limit=1000" >"$dir/verdicts.json"
  jqc -r '
    ["time","service","source","revision","checkpoint","outcome","verdict","partial","failing_signals","notes"],
    (.verdicts | reverse | .[] |
      [ .time, .service, .source, .revision, .checkpoint, .outcome, (.verdict // ""), (.partial // false),
        ([.signals[]? | select(.status != "ok" and .status != "not_evaluated") | "\(.name)=\(.status)"] | join(" ")),
        ((.notes // []) | join(" | ")) ])
    | @csv' <"$dir/verdicts.json" >"$dir/verdicts.csv"
  docker cp rikugan-lab-serve:/var/lib/rikugan "$dir/journal"
  cp state/actions.jsonl "$dir/"
  docker logs rikugan-lab-serve >"$dir/serve.log" 2>&1
  docker logs rikugan-lab-loadgen >"$dir/loadgen.log" 2>&1
  tar -czf "$dir.tar.gz" -C export "$(basename "$dir")"
  echo "exported to $dir.tar.gz"
}

cmd_down() {
  $SHOP down
  $LAB down
  record down "" "" ""
}

cmd_reset() {
  printf 'Delete the lab, its Prometheus data and rikugan journal? [y/N] '
  read -r answer
  [ "$answer" = y ] || die "nothing deleted"
  $SHOP down -v || true
  $LAB down -v || true
  rm -rf state
  echo "lab deleted; .env and export/ kept"
}

cmd_scenarios() {
  for s in healthy errors minor-errors slow slightly-slow crash oom leak; do
    printf '%-14s %s\n' "$s" "$(scenario "$s" | sed -n 's/^expected: //p')"
  done
}

cmd_help() {
  cat <<'EOF'
./lab.sh up                          start the lab (builds the images the first time)
./lab.sh build                       rebuild the images, e.g. after a git pull
./lab.sh deploy <web|api> <scenario> deploy a new revision (./lab.sh scenarios)
./lab.sh chain <svc> <count> <min>   deploy healthy revisions back to back
./lab.sh traffic <svc> <rps>         change a service's mean traffic
./lab.sh prometheus <stop|start>     simulate a metrics outage
./lab.sh verdicts [since]            rikugan's verdicts (default: last 24h)
./lab.sh status                      containers, scenarios, last verdicts
./lab.sh export                      verdicts, journal and actions, as a .tar.gz
./lab.sh down                        stop the lab, keep its data
./lab.sh reset                       delete the lab and its data
EOF
}

command=${1:-help}
[ $# -eq 0 ] || shift
case "$command" in
up | build | deploy | chain | traffic | prometheus | verdicts | status | export | down | reset | scenarios | help)
  "cmd_$command" "$@"
  ;;
*) cmd_help; exit 1 ;;
esac
