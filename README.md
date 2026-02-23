# MultiScan

MultiScan is a distributed TCP scanner with two components:

- Controller server: durable job coordinator with retry and lease-based assignment.
- Agent: synchronous worker over a persistent WebSocket connection.

## What is implemented

- Persistent job/result storage in SQLite (`DB_PATH`, default `./data/state.db`)
  - Uses normalized relational tables (`jobs`, `job_order`, `results`, `agents`, `meta`).
  - JSON blob persistence has been removed.
  - On first startup, server auto-imports legacy state from:
    - prior SQLite blob table (`multiscan_state`) if present
    - otherwise `LEGACY_STATE_FILE` (default `./data/state.json`) if present
- Distributed coordination with lease ownership:
  - Jobs are leased to one agent at a time.
  - Expired leases are automatically re-queued.
- Retry policy:
  - `max_attempts` per job (default `3`)
  - On scan failure or lease timeout, job is retried until max attempts then marked `failed`.
- Real WebSocket channel:
  - Agent maintains a persistent `/ws` connection.
  - Agent sends `SyncRequest` messages (optionally carrying completion from prior job).
  - Server replies with next work or wait instruction.
- In-task heartbeat:
  - While scanning, agents post periodic heartbeats to `/heartbeat`.
  - Dashboard `last_seen` updates during long-running scans.
- Efficient result retention:
  - Only open/connectable endpoints are retained in job results.
  - Closed/timeout endpoints are not persisted in coordinator state.
- Automatic job batching:
  - Submitted port ranges are split into sub-jobs of `1024` ports each.
  - This spreads work across multiple available agents.
  - Sub-jobs are hidden from the main job list.
  - Main job rows show summary counters (open ports, pending/active/completed sub-jobs).
- Top-ports scan mode:
  - Submit with `top_1000=true` to scan a top-1000 TCP port list (nmap-style).
  - Or submit with `top_n=<N>` to scan top-N ports (e.g. `5000`), which is split into 1024-port sub-jobs.
  - Jobs can carry explicit `ports[]`; agents consume that list directly.
  - If `/usr/share/nmap/nmap-services` is available, top ports are derived from it; otherwise fallback is ports `1..N`.
- Built-in server UI:
  - Open `http://localhost:8080/` to submit jobs and monitor agents/jobs in real time.
  - Use the `View` button on a job row to see open-port results ordered by IP then port.
  - Uses JSON APIs from the same server process.

## Project layout

- `cmd/server`: controller executable
- `cmd/agent`: agent executable
- `internal/protocol`: shared API/message types
- `internal/server`: durable store, leasing, retries, and HTTP/WS handlers
- `internal/scanner`: TCP scan engine
- `internal/wsproto`: minimal RFC6455 websocket transport implementation

## Run

1. Start the controller:

```bash
SERVER_ADDR=:8080 \
DB_PATH=./data/state.db \
LEGACY_STATE_FILE=./data/state.json \
LEASE_DURATION=2m \
SYNC_WAIT=25s \
go run ./cmd/server
```

2. Start one or more agents:

```bash
AGENT_ID=agent-a SERVER_URL=http://localhost:8080 go run ./cmd/agent
```

Optional explicit websocket URL override:

```bash
WS_URL=ws://localhost:8080/ws AGENT_ID=agent-b go run ./cmd/agent
```

Optional heartbeat tuning:

```bash
HEARTBEAT_INTERVAL=5s HEARTBEAT_URL=http://localhost:8080/heartbeat AGENT_ID=agent-c go run ./cmd/agent
```

Optional websocket reconnect tuning:

```bash
WS_READ_TIMEOUT=40s WS_WRITE_TIMEOUT=10s RETRY_DELAY=2s AGENT_ID=agent-d go run ./cmd/agent
```

3. Open dashboard UI:

```bash
open http://localhost:8080/
```

4. Submit a job (CLI alternative):

```bash
curl -s -X POST http://localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{
    "start_ip":"127.0.0.1",
    "end_ip":"127.0.0.1",
    "start_port":20,
    "end_port":1024,
    "max_attempts":4
  }'
```

Top-1000 mode:

```bash
curl -s -X POST http://localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{
    "start_ip":"127.0.0.1",
    "end_ip":"127.0.0.1",
    "top_1000":true,
    "max_attempts":3
  }'
```

Top-N mode:

```bash
curl -s -X POST http://localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{
    "start_ip":"127.0.0.1",
    "end_ip":"127.0.0.1",
    "top_n":5000,
    "max_attempts":3
  }'
```

5. Check status/results:

```bash
curl -s http://localhost:8080/work/job-0001
```

## API summary

- `POST /jobs`: enqueue scan job
- `GET /ws`: websocket upgrade endpoint for agents
- `POST /sync`: HTTP fallback sync endpoint
- `POST /heartbeat`: agent heartbeat update (used during active scans)
- `GET /api/jobs`: list all jobs
- `POST /api/jobs`: enqueue scan job (UI uses this)
- `GET /api/agents`: list agent status/heartbeat
- `GET /work/{jobID}`: fetch job status and results
- `GET /healthz`: health check
- `GET /`: browser dashboard

## Notes

- Scanner performs TCP connect scans only.
- IPv4 ranges are supported.
- Server storage uses local `sqlite3` command-line binary, which must be available on the host.
