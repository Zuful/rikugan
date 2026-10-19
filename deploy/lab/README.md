# Local lab

A small shop that runs on your machine, under real traffic, with a
Prometheus and `rikugan serve` watching it. You deploy good and bad
revisions, rikugan judges them at +5, +15 and +30 minutes and sweeps
every service every hour, and you compare what it says with what you
broke. Everything runs in Docker; nothing is exposed beyond 127.0.0.1.

```
 rikugan-lab (compose.yaml)               lab-shop (compose.shop.yaml)
 ┌─────────────┐  scrapes every replica  ┌────────────────────────────┐
 │ prometheus  │ ──────────────────────▶ │ web     busy, daily cycle  │──┐
 └─────────────┘                         │ api     moderate traffic   │──┤ call
 ┌─────────────┐  docker compose ps,     │ low     ~10 requests/min   │  │
 │ rikugan     │  inspect, events        │ worker  no HTTP traffic    │  │
 │ serve       │ ─ (Docker socket) ─▶    │ db      shared dependency  │◀─┘
 └─────────────┘                         └────────────────────────────┘
       │ Telegram / Slack                   ▲ web, api, low
 ┌─────────────┐                            │
 │ loadgen     │ ───────────────────────────┘
 └─────────────┘
```

Every service is the instrumented app of the end-to-end test
(`test/e2e/app`), with a profile of its own:

| Service | Traffic | What it is for |
|---|---|---|
| `web` | ~20 req/s, a daily cycle (×0.5 at night, ×1.5 at 14:00 UTC), bursts | a busy API: false alarms on normal variation |
| `api` | ~3 req/s | a moderate one |
| `low` | ~10 req/min | under the volume guard (200 requests per window) most of the time: how often is it *insufficient data*? |
| `worker` | none, a queue consumer | no request metrics at all: only restarts, availability and resources |
| `db` | what web and api send it | the dependency they share: take it down and both fail together |

`web`, `api` and `low` also carry background noise, what a healthy
service does anyway: 0.2 % of sporadic 5xx, 1 % of requests 250 ms
slower, a 150 ms pause every 2 minutes (as a GC would), and 90-second
bursts at three times the traffic every 3 hours on average. False
positives come from there. Each scenario changes a service's
environment: a 5xx rate, extra latency, a crash at start, an OOM, a slow
leak.

## 1. Before you start

- Docker Desktop (macOS: 4.x, Apple Silicon or Intel), with *Settings →
  Advanced → Allow the default Docker socket to be used* on: rikugan
  reads the shop's containers through `/var/run/docker.sock`.
- About 2 GB of memory for Docker and 3 GB of disk.
- Optional, to be notified: a Telegram bot. Create one with
  [@BotFather](https://t.me/BotFather), send it any message, then read
  your chat id at `https://api.telegram.org/bot<token>/getUpdates`
  (`"chat":{"id":…}`). A Slack incoming webhook works too.

## 2. Start the lab

```sh
cd deploy/lab
./lab.sh up          # first run: creates .env with an API token, builds, starts
```

After a `git pull`, `./lab.sh build` then `./lab.sh up` runs the new code.

Put your Telegram bot token and chat id in `.env`, then run
`./lab.sh up` again to apply them.

`./lab.sh status` shows the containers, each service's scenario and the
last verdicts. Prometheus is at http://127.0.0.1:9090: try
`sum by (service) (rate(http_requests_total[1m]))`.

**Wait 30 minutes before the first deploy.** Rikugan compares each
rollout with the 30 minutes before it, and Prometheus starts empty.

## 3. Deploy

```sh
./lab.sh deploy web errors     # a new revision of web, answering 30 % of 5xx
./lab.sh scenarios             # what each scenario does, and what rikugan should say
```

| Scenario | The new revision | Expected |
|---|---|---|
| `healthy` | behaves | `ok` at +5, +15, +30; no message |
| `errors` | 30 % of 5xx | `degraded` on error rate from +5; one critical, not repeated |
| `minor-errors` | 2 % of 5xx | `investigate` on error rate; never `degraded` |
| `slow` | +300 ms per request | latency `investigate` at +5 (capped), `degraded` from +15; one critical |
| `slightly-slow` | +60 ms | latency `investigate`, never `degraded` |
| `crash` | exits at start | rollout failed at the 5-minute deadline, restarts degraded; one critical |
| `oom` | allocates until killed | rollout failed, restarts degraded, OOM kills reported |
| `transient-errors` | 10 % of 5xx for its first 7 minutes | `degraded` at +5 (critical); about 4.7 % at +15, so `investigate` and a "recovered" message |
| `leak` | retains 1 MiB more per minute | rollout `ok`; ~6 h later a sweep reports `memory_leak` investigate |

`worker` takes only `healthy`, `crash`, `oom` and `leak`. The after
window grows from the rollout's completion (+15 judges its first 15
minutes), which is why a 30 % burst of 5xx for 7 minutes would still be
degraded at +30: `transient-errors` is sized so that it recovers.

After a bad revision, `./lab.sh deploy web healthy` is the fix-forward:
rikugan judges it as a rollout of its own, and records the bad one's
remaining checkpoints as `superseded`.

Other levers:

```sh
./lab.sh traffic web 0.5       # very low traffic: under 200 requests per 5-minute window
./lab.sh dependency stop       # db down: web and api fail together; dependency start ends it
./lab.sh prometheus stop       # a metrics outage; ./lab.sh prometheus start ends it
./lab.sh prometheus wipe       # lose Prometheus' data: no J-1 or J-7 baseline left
./lab.sh chain web 10 15       # 10 healthy deploys, 15 minutes apart
./lab.sh phase                 # where the daily cycle stands: deploy at a peak, or in a trough
./lab.sh verdicts 6h           # every verdict of the last 6 hours
```

- **Shared dependency down:** web and api answer 503. Their sweeps go
  degraded together and send one grouped message; a rollout of either
  during the outage is judged on it too.

- **Low traffic:** latency and error rate become insufficient data;
  only the blunt rule (5xx above 20 % with at least 10 errors) can still
  flag a rollout. Try `traffic web 0.5`, then `deploy web errors`.
- **Prometheus down:** checks wait and retry. A crash or OOM is still
  notified from the containers alone (marked "metrics down"). Past 2 h
  after a rollout's completion its checks are given up, with a single
  "rikugan cannot evaluate" warning, and "rikugan evaluates again" when
  Prometheus is back.
- **Chained deploys:** a rollout superseded before its checkpoints is
  recorded `superseded`; after 2 h of back-to-back deploys there is no
  credible "before" window left, and latency and errors are not compared.

## 4. A first session

A suggested half-day, one deploy at a time, each after the previous
one's +30 checkpoint:

1. `up`, then wait 30 min.
2. `deploy web healthy`: no message is the expected outcome.
3. `deploy web errors`, then `deploy web healthy` after the critical.
4. `deploy api slow`, then `deploy api healthy`.
5. `deploy web crash`, then `deploy web healthy` once it has failed.
6. `deploy api minor-errors`, then `deploy api healthy`.
7. `deploy web leak` in the evening, and leave the lab running overnight.

Then leave it running for a few days: the hourly sweeps compare each
hour with the same hour a day earlier (a week earlier once there is a
week of data), and a quiet week tells how often rikugan raises a false
alarm on traffic that only follows its daily cycle and random bursts.

Then, deploy at the peak and in the trough of the day
(`./lab.sh phase`): the traffic signal compares the after window with
the before one and with the same time a week earlier.

## 5. Export the results

```sh
./lab.sh export
```

writes `export/<date>.tar.gz` with:

- `verdicts.json` and `verdicts.csv`: every check and sweep of the last
  30 days, with its signals and notes;
- `journal/`: rikugan's full journal, notifications included;
- `actions.jsonl`: the ground truth, every `lab.sh` action with its
  time, what was really injected (`severity`, `signals`, `lasts`, the
  `services` affected), the traffic at that moment as a multiple of its
  mean, and how rikugan is designed to react (`expected`). The truth is
  what happened, not what rikugan can see: a 2 % error rate on `low`,
  which rikugan has too few requests to judge, still counts as missed;
- `serve.log`, `loadgen.log`.

## 6. Settings

`.env` holds the traffic (`LAB_WEB_RPS`, `LAB_API_RPS`, `LAB_LOW_RPS`,
`LAB_CYCLE`, `LAB_AMPLITUDE`, `LAB_PEAK_AT`, `LAB_BURST_EVERY`,
`LAB_BURST_LENGTH`), the background noise (`LAB_NOISE_ERROR_RATE`,
`LAB_TAIL_SHARE`, `LAB_TAIL_MS`, `LAB_PAUSE_EVERY`, `LAB_PAUSE_MS`),
the shop's memory limit (`LAB_MEM_LIMIT`), the rollout deadline (5 min here,
10 by default), the sweep interval (1 h here, 4 h by default) and the
host ports. `health.yaml` is rikugan's health configuration: the request
templates are the defaults, the memory and CPU templates read the app's
process metrics instead of Kubernetes' cAdvisor ones. Run `./lab.sh up`
after a change.

## 7. Stop

```sh
./lab.sh down     # stop, keep Prometheus' data and rikugan's journal
./lab.sh reset    # delete everything except .env and export/
```

## Troubleshooting

- **`compose rollouts: cannot list services` in `docker logs
  rikugan-lab-serve`:** rikugan cannot reach the Docker socket; check the
  Docker Desktop setting above.
- **Port 9090 or 8080 already taken:** change `LAB_PROMETHEUS_PORT` or
  `LAB_RIKUGAN_PORT` in `.env`.
- **No Telegram message:** `docker logs rikugan-lab-serve` says whether
  the channel was configured and whether a send failed.
