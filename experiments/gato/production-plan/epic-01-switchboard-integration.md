# Epic 01: Switchboard Integration

## Business Meaning

Gato earns the right to receive sessions from Voqalcloud by connecting to the Switchboard as a registered node. Without this, Gato is an island — no calls reach it. This epic makes Gato a first-class citizen of the Voqalcloud platform: it registers, receives probes, accepts or rejects sessions based on capacity, relays WebRTC signaling, and disconnects cleanly when draining.

---

## Background

The Switchboard assigns browser sessions to agent nodes over a binary WebSocket protocol. The wire format is 4-byte big-endian length prefix + protobuf payload. Gato must implement the **node** side of this protocol.

**Existing references:**
- Protocol definition: `switchboard/proto/switchboard.proto`
- Node handler (Switchboard server side): `switchboard/internal/node/handler.go`
- Python reference implementation: `agent-sdk/src/voqalcloud/worker/_app.py` (reconnect loop), `_process_manager.py` (probe handling)
- Named types and state machine patterns: `switchboard/internal/state/`

**Session state machine (node perspective):**
```
IDLE → (SessionProbe received) → PROBING → (SessionAck sent) → NEGOTIATING
     → (WebRTCConnected sent) → (SessionReady received) → LIVE
     → (user hangs up or SessionRelease received) → IDLE
```

---

## Tasks

### Task 1.1 — Module scaffold and proto compilation

Create `gato/` as a new Go module in the monorepo: `module github.com/recruit41/voqalcloud/gato`.

Dependencies: `nhooyr.io/websocket`, `google.golang.org/protobuf`, `github.com/rs/zerolog`, `github.com/prometheus/client_golang`. Match exact versions from `switchboard/go.mod` to keep the dependency graph consistent.

Copy or symlink the compiled proto file: `switchboard/internal/proto/switchboard.pb.go` → `gato/internal/proto/switchboard.pb.go`. Add a `make proto` target that recompiles from `switchboard/proto/switchboard.proto` so both services stay in sync.

Add `Makefile` targets: `build`, `test`, `test-integration`, `proto`, `docker-build`.

Subtask: add a `cmd/gato/main.go` with `main()` that loads config, logs startup, and exits. Nothing else yet. This is the skeleton that all later epics will hang off.

### Task 1.2 — Config package

Create `gato/internal/config/config.go` following the exact pattern from `switchboard/internal/config/config.go`.

Required env vars for Gato:

| Env Var | Description |
|---------|-------------|
| `GATO_LISTEN_ADDR` | HTTP listen address (default `:8390`) |
| `GATO_SB_URL` | Switchboard WebSocket URL (`wss://...`) |
| `GATO_SB_TOKEN` | Bearer token for Switchboard node auth |
| `GATO_DEPLOYMENT_ID` | This node's deployment ID |
| `GATO_NODE_ID` | Stable node identity (default: `hostname`) |
| `GATO_MAX_SESSIONS` | Max concurrent sessions (default: `20`) |
| `GATO_VERSION` | Binary version string (embedded at build time) |
| `GATO_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `GATO_LOG_FORMAT` | `json` or `console` |
| `GATO_T_SHUTDOWN_MS` | Drain timeout in ms (default `8000`) |

`config.Validate()` must fail fast with a clear error if `GATO_SB_URL`, `GATO_SB_TOKEN`, or `GATO_DEPLOYMENT_ID` are empty.

Add `.env.gato.example` with safe placeholder values.

### Task 1.3 — Switchboard WebSocket client

Create `gato/internal/switchboard/client.go`. This is the heart of the integration.

The client runs a **reconnect loop** (same pattern as `agent-sdk/_app.py`):
- Base backoff 100ms, doubling, capped at 60s, reset on successful connection > 30s
- On each connect attempt:
  1. Dial `GATO_SB_URL/ws/node` with `Authorization: Bearer <GATO_SB_TOKEN>`
  2. Send `RegisterNodeRequest{node_id, deployment_id, version, max_sessions, active_sessions}` within 10s
  3. Read `RegisterNodeResponse`; on failure, backoff and retry
  4. Start three goroutines: `pingLoop`, `recvLoop`, and `sendLoop` (from a channel)

**pingLoop**: every 30s, send `NodePing{timestamp_ms, active_sessions, accepting_jobs}`. `accepting_jobs` is false when draining.

**recvLoop**: read frames from the WebSocket, unmarshal `SwitchboardMessage`, dispatch by `oneof payload` to handlers:
- `SessionProbe` → `handleProbe`
- `SessionReady` → `handleSessionReady`
- `SessionRelease` → `handleSessionRelease`
- `SessionTermination` → `handleSessionTermination`
- `BrowserToAgentSignal` → `handleBrowserSignal`
- `NodePong` → reset ping timeout

**sendLoop**: reads `NodeMessage` from a buffered channel and writes to the WebSocket. All outbound messages go through this single goroutine so the WebSocket writer is never accessed concurrently.

Frame framing: `writeFrame(conn, msg)` marshals the protobuf message, prepends a 4-byte big-endian length, and writes the concatenated buffer in a single WebSocket binary message. `readFrame(conn)` reads one WebSocket message, strips the 4-byte prefix, and unmarshals. See `switchboard/internal/node/handler.go` for the server-side framing reference.

### Task 1.4 — Session probe handler

`handleProbe(sessionID SessionID)` is the capacity gate:

1. Check `activeSessionCount < maxSessions && !draining`
2. If yes: atomically reserve a slot, send `SessionAck{session_id}`; create a `PendingSession` entry in the session table
3. If no: send `SessionNack{session_id, reason: "at capacity"}`

`SessionID` must be a named type (`type SessionID string`), not a bare string — follow `switchboard/internal/state/` conventions.

The session table must be safe for concurrent access. Use `sync.RWMutex` on a map, not `sync.Map` (the switchboard prefers explicit mutex + map for readability).

### Task 1.5 — Signal relay

When Gato sends an SDP answer back to the browser, it goes via `AgentToBrowserSignal{session_id, payload, is_text: true}`.

When the browser sends an ICE candidate or SDP update, Switchboard delivers `BrowserToAgentSignal{session_id, payload}`. The `handleBrowserSignal` dispatches to the correct session's signal channel (`chan []byte` per session in the session table).

Each session reads from its signal channel in the WebRTC negotiation goroutine (Epic 02). This decouples the Switchboard recv goroutine from the per-session WebRTC setup.

### Task 1.6 — Session release and termination

`SessionRelease`: the browser has disconnected normally. Mark the session as done; wake up the session goroutines via `session.cancel()`. Decrement `activeSessionCount`.

`SessionTermination`: Switchboard is force-closing the session (e.g., payment failure, policy). Same cleanup as release, but log at warn level.

Both paths must call `session.Wait()` before returning the goroutine slot — ensure no goroutine leak.

### Task 1.7 — Drain and reconnect handoff

On SIGTERM (handled by `gato/internal/shutdown`):
1. Set `draining = true`
2. Stop sending `NodePing` with `accepting_jobs: true`
3. Serve `/ready` with HTTP 503 (signals load balancer to stop sending new sessions)
4. Wait up to `T_SHUTDOWN` for active sessions to finish
5. Force-cancel remaining sessions
6. Close WebSocket connection (sends close frame)
7. Exit 0

If the WebSocket connection drops mid-operation (Switchboard restart), the client must reconnect without losing track of active sessions. The reconnect `RegisterNodeRequest` includes `active_sessions: <count>` so Switchboard can reconcile.

---

## Definition of Done

- [ ] `gato/` module compiles with `go build ./...`
- [ ] Gato connects to a real Switchboard instance in the development environment, logs `registered node_id=... deployment_id=... max_sessions=...`
- [ ] Probe → Ack/Nack cycle observed in logs when a browser connects
- [ ] `SessionRelease` decrements active session count (verified via `/health` endpoint)
- [ ] SIGTERM triggers drain: no new probes accepted; existing sessions complete; process exits 0
- [ ] Reconnect loop: kill the Switchboard process, restart it; Gato reconnects within 60s without manual intervention
- [ ] No goroutine leak on repeated connect/disconnect cycles (verify with `runtime.NumGoroutine()` in test)

---

## Verification

### Unit Tests

- `TestProbeHandler_AtCapacity`: stub the send channel; call `handleProbe` when `activeSessionCount == maxSessions`; assert `SessionNack` emitted
- `TestProbeHandler_UnderCapacity`: assert `SessionAck` emitted and session table has one entry
- `TestSignalRelay`: inject a `BrowserToAgentSignal` message; assert the signal arrives on the session's signal channel
- `TestDrain_RejectsNewProbes`: set draining=true; call handleProbe; assert Nack and count unchanged
- `TestReadFrame_BadLength`: send a 2-byte message; assert error, no panic
- `TestWriteFrame_RoundTrip`: write + read a `RegisterNodeRequest`; assert proto equality

### Integration Tests (`tests/integration/`)

Use a `FakeSwitchboard` (real TCP listener, real WebSocket, real proto framing — no mocks). Mirror the pattern from `switchboard/tests/integration/` `FakeNode`:

1. Start `FakeSwitchboard` on a random port
2. Start Gato client pointed at it
3. Assert `RegisterNodeRequest` received within 5s
4. Send `SessionProbe{session_id: "s1"}`; assert `SessionAck` received
5. Send `SessionRelease{session_id: "s1"}`; assert `activeSessionCount` drops to 0 (via `/health` endpoint)
6. Kill FakeSwitchboard; wait 2s; restart on same address; assert Gato reconnects and re-registers

### Server E2E

Start a real Switchboard (dev environment) + real Gato binary. Open a browser session via the console. Observe in Gato logs: `probe`, `ack`, `session_ready`, `session_release`. Confirm no orphan goroutines via `GET /debug/pprof/goroutine`.
