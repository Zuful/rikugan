#!/usr/bin/env bash
# Drives the local lab: see README.md, or ./lab.sh help.
# Written for the bash 3.2 macOS ships.
set -euo pipefail

cd "$(dirname "$0")"
LAB="docker compose -f compose.yaml"
SHOP="docker compose -f compose.shop.yaml"
JQ_IMAGE=ghcr.io/jqlang/jq:1.7.1
SERVICES="web api low worker db"
HTTP_SERVICES="web api low db"
SCENARIOS="healthy errors minor-errors transient-errors slow slightly-slow crash oom leak"

die() { echo "lab: $*" >&2; exit 1; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# record appends an action to state/actions.jsonl: the ground truth the
# scoring compares rikugan's verdicts with. severity and signals say
# what was really injected (none, investigate, degraded), whatever
# rikugan can see of it; lasts is how long it lasts when it stops by
# itself; services are those it affects; expected is how rikugan is
# designed to react, for the reader.
record() { # action service scenario severity signals lasts expected [services]
  mkdir -p state
  signals=""
  [ -z "$5" ] || signals="\"$(echo "$5" | sed 's/,/","/g')\""
  affected=""
  [ -z "${8:-$2}" ] || affected="\"$(echo "${8:-$2}" | sed 's/,/","/g')\""
  printf '{"time":"%s","action":"%s","service":"%s","scenario":"%s","severity":"%s","signals":[%s],"lasts":"%s","services":[%s],"traffic":%s,"expected":"%s"}\n' \
    "$(now)" "$1" "$2" "$3" "$4" "$signals" "$6" "$affected" "$(traffic_factor)" "$7" >>state/actions.jsonl
  echo "[$(now)] $1 $2 $3"
  [ -z "$7" ] || echo "  expected: $7"
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

in_list() { case " $2 " in *" $1 "*) return 0 ;; esac; return 1; }

check_service() { in_list "$1" "$SERVICES" || die "unknown service '$1': want one of $SERVICES"; }

# seconds parses a Go-style duration made of h, m and s parts.
seconds() {
  echo "$1" | awk '{
    s = 0; n = ""
    for (i = 1; i <= length($0); i++) {
      c = substr($0, i, 1)
      if (c ~ /[0-9.]/) { n = n c; continue }
      if (c == "h") s += n * 3600; else if (c == "m") s += n * 60; else if (c == "s") s += n
      n = ""
    }
    print int(s)
  }'
}

# cycle_awk runs an awk program with t (seconds past the last peak, in
# the daily cycle), c (the cycle) and a (the amplitude), as the loadgen
# computes them: a cosine on the wall clock, peaking at LAB_PEAK_AT UTC.
cycle_awk() {
  c=$(seconds "$(env_value LAB_CYCLE)")
  p=$(seconds "$(env_value LAB_PEAK_AT)")
  a=$(env_value LAB_AMPLITUDE)
  awk -v now="$(date +%s)" -v c="$c" -v p="${p:-50400}" -v a="${a:-0.5}" \
    "BEGIN { t = (c > 0) ? (now - p) % c : 0; $1 }"
}

# traffic_factor prints the loadgen's current rate as a multiple of its
# mean (the daily cycle, bursts aside).
traffic_factor() { cycle_awk 'printf "%.2f\n", (c > 0) ? 1 + a * cos(2 * 3.14159265 * t / c) : 1'; }

# scenario prints the environment of a scenario, then two lines:
#   truth: <severity> [<signals> [<lasts>]]  what is really injected
#   expected: <text>                         how rikugan should react
scenario() {
  case "$1" in
  healthy)
    echo "truth: none"
    echo "expected: ok at +5, +15 and +30; no message"
    ;;
  errors)
    echo "ERROR_RATE=0.3"
    echo "truth: degraded error_rate"
    echo "expected: degraded (error_rate) from +5; one critical, not repeated at +15/+30"
    ;;
  minor-errors)
    echo "ERROR_RATE=0.02"
    echo "truth: investigate error_rate"
    echo "expected: investigate (error_rate); never degraded"
    ;;
  transient-errors)
    echo "ERROR_RATE=0.1"
    echo "ERROR_FOR=7m"
    echo "truth: degraded error_rate 7m"
    echo "expected: degraded at +5 (critical); the after window grows from the completion, so about 4.7 % of 5xx at +15: investigate, and a recovered message"
    ;;
  slow)
    echo "LATENCY_MS=300"
    echo "truth: degraded latency"
    echo "expected: latency investigate at +5 (capped), degraded from +15; one critical"
    ;;
  slightly-slow)
    echo "LATENCY_MS=60"
    echo "truth: investigate latency"
    echo "expected: latency investigate (about +60 ms); never degraded"
    ;;
  crash)
    echo "CRASH=true"
    echo "truth: degraded restarts,availability"
    echo "expected: rollout failed at the 5 min deadline, restarts degraded; one critical. Fix with: deploy <service> healthy"
    ;;
  oom)
    echo "OOM=true"
    echo "truth: degraded restarts,availability"
    echo "expected: rollout failed, restarts degraded with OOM kills; one critical"
    ;;
  leak)
    echo "LEAK_MB_PER_MIN=1"
    echo "truth: investigate memory_leak"
    echo "expected: rollout ok; after about 6 h, a sweep reports memory_leak investigate"
    ;;
  *) return 1 ;;
  esac
}

field() { scenario "$1" | sed -n "s/^$2: //p"; }

write_env() { # service scenario
  {
    echo "# written by lab.sh: scenario $2"
    echo "LAB_SCENARIO=$2"
    echo "LAB_DEPLOYED_AT=$(date +%s)"
    scenario "$2" | grep -v -e '^expected:' -e '^truth:' || true
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
  record up "" "" none "" "" ""
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
  [ $# -eq 2 ] || die "usage: ./lab.sh deploy <service> <scenario>  (./lab.sh scenarios lists them)"
  check_service "$1"
  scenario "$2" >/dev/null || die "unknown scenario '$2': ./lab.sh scenarios lists them"
  if ! in_list "$1" "$HTTP_SERVICES" && ! in_list "$2" "healthy crash oom leak"; then
    die "$1 serves no HTTP traffic: healthy, crash, oom or leak only"
  fi
  write_env "$1" "$2"
  $SHOP up -d "$1"
  # shellcheck disable=SC2046 # the truth line splits into its fields
  set -- "$1" "$2" $(field "$2" truth)
  record deploy "$1" "$2" "$3" "${4:-}" "${5:-}" "$(field "$2" expected)"
}

cmd_chain() { # service count minutes
  [ $# -eq 3 ] || die "usage: ./lab.sh chain <service> <count> <minutes between deploys>"
  check_service "$1"
  record chain "$1" "$2 deploys every $3 min" none "" "" "rollouts superseded before their checkpoints are recorded superseded; past 2 h of chaining, latency and errors are not compared (no credible baseline)"
  i=1
  while [ "$i" -le "$2" ]; do
    write_env "$1" healthy
    $SHOP up -d "$1"
    record deploy "$1" healthy none "" "" "part of a chain ($i/$2)"
    [ "$i" -eq "$2" ] || sleep $(($3 * 60))
    i=$((i + 1))
  done
}

cmd_traffic() { # service rps
  [ $# -eq 2 ] || die "usage: ./lab.sh traffic <web|api|low> <requests per second>"
  case "$1" in
  web) key=LAB_WEB_RPS ;;
  api) key=LAB_API_RPS ;;
  low) key=LAB_LOW_RPS ;;
  *) die "the loadgen sends traffic to web, api and low only" ;;
  esac
  set_env "$key" "$2"
  $LAB up -d loadgen
  record traffic "$1" "$2 rps" none "" "" "below about 0.7 rps, a 5-minute window has under 200 requests: latency and errors become insufficient data, and only the blunt rule (5xx above 20 %) can flag a rollout"
}

cmd_dependency() { # stop|start
  case "${1:-}" in
  stop)
    $SHOP stop db
    record dependency-stop db "" degraded error_rate "" "web and api answer 503: degraded on error rate in their sweeps (one grouped message), and in the checks of any rollout during the outage" web,api
    ;;
  start)
    $SHOP start db
    record dependency-start db "" none "" "" "web and api recover: resolved after two ok sweeps" web,api
    ;;
  *) die "usage: ./lab.sh dependency <stop|start>" ;;
  esac
}

cmd_prometheus() { # stop|start|wipe
  case "${1:-}" in
  stop)
    $LAB stop prometheus
    record prometheus-stop "" "" none "" "" "checks wait; past 2 h after a completion they are given up, with one 'rikugan cannot evaluate' warning; a crash is still notified (partial check)"
    ;;
  start)
    $LAB start prometheus
    record prometheus-start "" "" none "" "" "an 'rikugan evaluates again' message if a warning was sent"
    ;;
  wipe)
    printf 'Delete all of Prometheus data (the J-1 and J-7 baselines with it)? [y/N] '
    read -r answer
    [ "$answer" = y ] || die "nothing deleted"
    $LAB rm -sf prometheus
    docker volume rm rikugan-lab_prometheus-data >/dev/null
    $LAB up -d prometheus
    record prometheus-wipe "" "" none "" "" "sweeps find no J-7 or J-1 baseline: insufficient data until a day of data is back; rollouts need 30 minutes of data before them"
    ;;
  *) die "usage: ./lab.sh prometheus <stop|start|wipe>" ;;
  esac
}

# prom_query prints the first value of a PromQL instant query, or
# nothing.
prom_query() {
  curl -fsS "http://127.0.0.1:$(prom_port)/api/v1/query" --data-urlencode "query=$1" 2>/dev/null |
    sed -n 's/.*"value":\[[^,]*,"\([^"]*\)"\].*/\1/p'
}

# cmd_history loads a synthetic past into Prometheus, so that sweeps
# have their J-1 and J-7 baselines from the first day. It ends where
# the lab's own data begins, so the two never overlap.
cmd_history() { # [days]
  days=${1:-8}
  case "$days" in '' | *[!0-9]*) die "usage: ./lab.sh history [days]" ;; esac
  [ -n "$(prom_query 'vector(1)')" ] || die "Prometheus does not answer: is the lab up?"
  [ -z "$(prom_query 'count(http_requests_total{instance="history"})')" ] ||
    die "a history is already loaded: ./lab.sh prometheus wipe first"
  first=$(prom_query 'min(min_over_time(timestamp(up{job="shop"})[9d:1m]))')
  end=$(date +%s)
  [ -z "$first" ] || end=$(echo "$first" | cut -d. -f1)
  end=$((end - 60))
  web=$(env_value LAB_WEB_RPS)
  api=$(env_value LAB_API_RPS)
  low=$(env_value LAB_LOW_RPS)
  echo "writing $days days of history, up to where the lab's own data begins (a minute to a few, and some memory: about 55 MB of samples a day)"
  $LAB stop prometheus
  docker run --rm \
    -e TARGETS="http://web:8080/work=${web:-20},http://api:8080/work=${api:-3},http://low:8080/work=${low:-0.17}" \
    -e CYCLE="$(env_value LAB_CYCLE)" -e AMPLITUDE="$(env_value LAB_AMPLITUDE)" -e PEAK_AT="$(env_value LAB_PEAK_AT)" \
    -e NOISE_ERROR_RATE="$(env_value LAB_NOISE_ERROR_RATE)" -e TAIL_SHARE="$(env_value LAB_TAIL_SHARE)" -e TAIL_MS="$(env_value LAB_TAIL_MS)" \
    rikugan-lab-app:local history -days "$days" -end "$end" |
    docker run --rm -i -v rikugan-lab_prometheus-data:/prometheus --entrypoint /bin/promtool prom/prometheus:v3.5.0 \
      tsdb create-blocks-from openmetrics /dev/stdin /prometheus
  $LAB start prometheus
  record history "" "$days days" none "" "" "sweeps compare with the same hour a day and a week earlier from now on"
}

cmd_phase() {
  cycle_awk '
    if (c <= 0) { printf "traffic flat at its mean: no daily cycle\n"; exit }
    f = 1 + a * cos(2 * 3.14159265 * t / c)
    peak = c - t; trough = (c / 2 - t + c) % c
    printf "traffic at %.2f × its mean; next peak in %dh%02d, next trough in %dh%02d\n",
      f, peak / 3600, (peak % 3600) / 60, trough / 3600, (trough % 3600) / 60'
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
  cmd_phase
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
  docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/$dir:/export" rikugan-lab-app:local score /export >/dev/null
  tar -czf "$dir.tar.gz" -C export "$(basename "$dir")"
  echo "exported to $dir.tar.gz; the score is in $dir/report.md"
}

cmd_down() {
  $SHOP down
  $LAB down
  record down "" "" none "" "" ""
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
  for s in $SCENARIOS; do
    printf '%-17s %s\n' "$s" "$(field "$s" expected)"
  done
  echo
  echo "web, api, low and db take any scenario; worker (no HTTP traffic) takes healthy, crash, oom and leak."
}

cmd_help() {
  cat <<'EOF'
./lab.sh up                           start the lab (builds the images the first time)
./lab.sh build                        rebuild the images, e.g. after a git pull
./lab.sh deploy <service> <scenario>  deploy a new revision (./lab.sh scenarios)
./lab.sh chain <svc> <count> <min>    deploy healthy revisions back to back
./lab.sh traffic <web|api|low> <rps>  change a service's mean traffic
./lab.sh dependency <stop|start>      take db, which web and api call, down
./lab.sh prometheus <stop|start|wipe> a metrics outage, or lose its data
./lab.sh history [days]               load a synthetic past (default 8 days) for the sweeps' baselines
./lab.sh phase                        where the daily traffic cycle stands
./lab.sh verdicts [since]             rikugan's verdicts (default: last 24h)
./lab.sh status                       containers, scenarios, last verdicts
./lab.sh export                       verdicts, journal, actions and the score, as a .tar.gz
./lab.sh down                         stop the lab, keep its data
./lab.sh reset                        delete the lab and its data

Services: web (busy, daily cycle), api, low (~10 req/min), worker (no
HTTP), db (the dependency of web and api).
EOF
}

command=${1:-help}
[ $# -eq 0 ] || shift
case "$command" in
up | build | deploy | chain | traffic | dependency | prometheus | history | phase | verdicts | status | export | down | reset | scenarios | help)
  "cmd_$command" "$@"
  ;;
*) cmd_help; exit 1 ;;
esac
