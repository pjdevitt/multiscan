# MultiScan Project Summary

This file is a handoff for future Codex sessions.

## Goal
Distributed TCP scanner with:
- Controller server (job orchestration, assignment, result aggregation, UI)
- Agents (pull work over WebSocket, scan, return results)

## Current Architecture

### Components
- `cmd/server`: HTTP/WebSocket control plane
- `cmd/agent`: worker/agent process
- `internal/server`: coordinator logic + persistence + UI handlers
- `internal/scanner`: TCP connect scanner engine
- `internal/wsproto`: custom minimal RFC6455 transport
- `internal/protocol`: shared request/response structs

### Persistence
- SQLite-backed via embedded Go driver (`modernc.org/sqlite`) through `database/sql` (no external `sqlite3` binary required).
- Main DB path: `DB_PATH` (default `/data/state.db` in container, `./data/state.db` local).
- Normalized tables:
  - `jobs`
  - `job_order`
  - `results`
  - `agents`
  - `meta`
- Legacy migration support:
  - old `multiscan_state` blob table (if present)
  - legacy JSON state file (`LEGACY_STATE_FILE`, default `./data/state.json`)

## Job Model

### Parent + Sub-job pattern
- User submits one logical job (parent).
- Server creates many sub-jobs for scheduling.
- UI main list shows parent summaries only; sub-jobs are hidden.

### Sub-job splitting
- Split by **IP** and **port batch**.
- Batch size is fixed at 1024 ports per sub-job.
- For IP range + top-N ports:
  - sub-jobs ~= `num_ips * ceil(num_ports / 1024)`

### Result retention
- Only open ports are retained/persisted.
- Closed/filtered/timeout endpoints are not stored.

### Ordering
- Returned results are sorted by IP then port.

## Scan Modes

### Range mode
- `start_port` + `end_port`

### Explicit list mode
- `ports: []`

### Top ports mode
- `top_1000: true` shortcut
- `top_n: <N>` general mode

Top ports source order:
1. `/usr/share/nmap/nmap-services` (or `NMAP_SERVICES_PATH`)
2. Built-in fallback list (explicit long nmap-style sequence)
3. If `top_n` exceeds fallback length, fill with remaining ports in numeric order

## Agent/Server Communication

### Primary channel
- WebSocket `GET /ws`
- Agent sends sync messages with optional completion payload.

### Fallback
- HTTP `POST /sync`

### Heartbeat
- HTTP `POST /heartbeat`
- During scans, agent sends periodic heartbeats.

### Reconnect behavior
- Agent has WS read/write deadlines and reconnect loop.
- Environment knobs: `WS_READ_TIMEOUT`, `WS_WRITE_TIMEOUT`, `RETRY_DELAY`.

## Security Controls

### Agent authentication
- Optional shared key gate for agent endpoints.
- Server env: `REQUIRED_CLIENT_KEY`
- Agent env: `CLIENT_KEY`
- Applied on: `/ws`, `/sync`, `/heartbeat`

### UI authentication
- Optional HTTP Basic Auth for browser-facing endpoints.
- Server env:
  - `UI_BASIC_AUTH_USER`
  - `UI_BASIC_AUTH_PASS`
- Applied on:
  - `/`
  - `/api/jobs`
  - `/api/agents`
  - `/work/{jobID}`

### Restricted network policy (agent capability)
- Agent advertises `ALLOW_RESTRICTED_NET_SCANS` (default false).
- Server only assigns restricted-target jobs to agents with this true.
- Restricted prefixes currently enforced:
  - `192.168.0.0/16`
  - `10.0.0.0/8`
  - `172.16.0.0/12`

## UI Features
- Create job form
- Job summary table:
  - open port count
  - sub-jobs pending/active/completed (and failed in API)
- Agent table:
  - state, current job, last seen, completion/failure counters
- Results viewer for selected job

## Docker/Deploy

### Files
- `Dockerfile` for server image
- `.dockerignore`

### State persistence
- Mount `/data` volume and set:
  - `DB_PATH=/data/state.db`
  - optional `LEGACY_STATE_FILE=/data/state.json`

### Traefik
- Expected deployment: TLS terminated by Traefik, service stays HTTP on 8080.
- Agent should use `https://` and `wss://` public URLs when proxied.

## Important Environment Variables

### Server
- `SERVER_ADDR`
- `DB_PATH`
- `LEGACY_STATE_FILE`
- `LEASE_DURATION`
- `SYNC_WAIT`
- `REQUIRED_CLIENT_KEY`
- `UI_BASIC_AUTH_USER`
- `UI_BASIC_AUTH_PASS`
- `NMAP_SERVICES_PATH`

### Agent
- `AGENT_ID`
- `SERVER_URL`
- `WS_URL`
- `HEARTBEAT_URL`
- `CLIENT_KEY`
- `ALLOW_RESTRICTED_NET_SCANS`
- `HEARTBEAT_INTERVAL`
- `WS_READ_TIMEOUT`
- `WS_WRITE_TIMEOUT`
- `RETRY_DELAY`

## Known Constraints / Technical Debt
- SQLite dependency tree is larger due embedded driver transitive modules.
- `Store` holds a process-lifetime DB handle and does not expose an explicit close method yet.
- No auth roles/scopes yet; shared client key is global.
- No unit tests yet (project compiles and packages pass `go test ./...` with no test files).

## Suggested Next Steps
1. Add role-based auth model (separate agent keys, UI users).
2. Add tests for:
   - assignment filtering by restricted CIDR flag
   - parent/sub-job aggregation
   - top-N fallback ordering
   - reconnect and heartbeat semantics
3. Add pagination for large result sets in UI/API.
4. Add integration tests for DB restart/migration paths (relational, blob table, legacy JSON).
