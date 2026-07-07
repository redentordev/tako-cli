# Scheduled Jobs Example

**Cron jobs without a cron container** — declare `kind: job` services and the
node agent runs them on schedule as one-off containers, records every run, and
lets you trigger them manually.

## What This Example Does

✓ Deploys a web app plus two scheduled jobs to your VPS
✓ `nightly-cleanup` runs at 03:00 UTC every day
✓ `hourly-report` runs hourly in `America/New_York` with a 10 minute timeout
✓ Run history and output are kept on the node — no log files to manage

## Quick Start

```bash
cp .env.example .env
# edit .env with your server IP
tako deploy
```

## Working With Jobs

```bash
tako jobs                    # list jobs with schedule, next run, last status
tako jobs runs nightly-cleanup   # run history: trigger, status, exit, duration
tako jobs trigger nightly-cleanup # run it right now, streaming output
tako logs nightly-cleanup    # output of the latest recorded run
```

## How It Works

- `kind: job` replaces long-running loop containers (`while true; do ...;
  sleep 3600; done`). The container only exists while the job runs.
- Schedules are five-field cron expressions or descriptors like `@hourly`,
  evaluated in UTC unless `timezone` is set.
- Overlapping runs are skipped and recorded as `skipped`; runs exceeding
  `timeout` (default 1h) are killed and recorded as `timeout`.
- On multi-node environments each job fires on exactly one node.
- Jobs support `env`, `envFile`, `volumes`, and `build:` like normal services.
