# Local lab

A small shop that runs on your machine, under real traffic, with a
Prometheus and `rikugan serve` watching it. You deploy good and bad
revisions, rikugan judges them at +5, +15 and +30 minutes and sweeps
every service every hour, and you compare what it says with what you
broke. Everything runs in Docker; nothing is exposed beyond 127.0.0.1.

```
 rikugan-lab (compose.yaml)                    lab-shop (compose.shop.yaml)
 ┌─────────────┐   scrapes every replica   ┌──────────┐
 │ prometheus  │ ────────────────────────▶ │ web  ×1  │◀──┐
 └─────────────┘                           │ api  ×1  │◀──┤ ~20 and ~3 req/s,
 ┌─────────────┐   docker compose ps,      └──────────┘   │ daily cycle, bursts
 │ rikugan     │   inspect, events                        │
 │ serve       │ ── (Docker socket) ──▶ rollouts    ┌─────┴─────┐
 └─────────────┘                                    │  loadgen  │
       │ Telegram / Slack                           └───────────┘
```

`web` and `api` are the instrumented app of the end-to-end test
(`test/e2e/app`), whose behaviour each scenario changes through its
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
| `leak` | retains 1 MiB more per minute | rollout `ok`; ~6 h later a sweep reports `memory_leak` investigate |

After a bad revision, `./lab.sh deploy web healthy` is the fix-forward:
rikugan judges it as a rollout of its own, and records the bad one's
remaining checkpoints as `superseded`.

Other levers:

```sh
./lab.sh traffic web 0.5       # very low traffic: under 200 requests per 5-minute window
./lab.sh prometheus stop       # a metrics outage; ./lab.sh prometheus start ends it
./lab.sh chain web 10 15       # 10 healthy deploys, 15 minutes apart
./lab.sh verdicts 6h           # every verdict of the last 6 hours
```

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

## 5. Export the results

```sh
./lab.sh export
```

writes `export/<date>.tar.gz` with:

- `verdicts.json` and `verdicts.csv`: every check and sweep of the last
  30 days, with its signals and notes;
- `journal/`: rikugan's full journal, notifications included;
- `actions.jsonl`: every `lab.sh` action with its time and the expected
  verdict, to line up with the verdicts;
- `serve.log`, `loadgen.log`.

## 6. Settings

`.env` holds the traffic (`LAB_WEB_RPS`, `LAB_API_RPS`, `LAB_CYCLE`,
`LAB_AMPLITUDE`, `LAB_BURST_EVERY`), the rollout deadline (5 min here,
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
