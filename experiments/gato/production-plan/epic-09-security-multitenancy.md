# Epic 09: Security & Multi-tenancy

## Business Meaning

Gato runs sessions for multiple customers simultaneously. A bug in one customer's agent code must not expose another customer's audio, transcripts, or session data. Secrets must be rotatable without downtime. Unauthorized nodes must not be able to connect to the Switchboard. This epic makes Gato safe to run in a shared cloud environment.

---

## Background

The Switchboard already enforces authentication at the node boundary:
- Node connection requires `Authorization: Bearer <GATO_SB_TOKEN>` (constant-time compare, no CP round-trip) — see `switchboard/internal/node/handler.go`
- Two-token rotation: `SB_WORKER_SECRET` + `SB_WORKER_SECRET_PREV` — see `switchboard/internal/config/config.go`

Gato must implement the same patterns for all its own endpoints and must ensure session-level isolation within the process.

---

## Tasks

### Task 9.1 — Bearer token validation for internal endpoints

Gato may expose an HTTP management API (for future admin use). Protect all non-public endpoints with Bearer token auth:

```go
func BearerAuth(token string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
            if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
                http.Error(w, "Unauthorized", http.StatusUnauthorized)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

`subtle.ConstantTimeCompare` prevents timing attacks. Never use `==` for token comparison.

Exempt from auth: `/health`, `/metrics`, `/ready`, `/debug/pprof/*` (the debug port is internal-only, never exposed to the public internet).

Config: `GATO_ADMIN_TOKEN` (required if admin API is enabled). Unset = admin API disabled.

### Task 9.2 — Secret rotation without downtime

Follow the switchboard's two-token rotation pattern for `GATO_SB_TOKEN`. On token rotation:

1. Deploy new binary with `GATO_SB_TOKEN=new-token` and `GATO_SB_TOKEN_PREV=old-token`
2. Gato presents `new-token` when connecting to Switchboard
3. If Switchboard accepts `new-token`, the rotation is complete
4. `GATO_SB_TOKEN_PREV` is only a fallback for the connection window

This is a deployment-time concern, not runtime logic. Document the rotation procedure in `gato/docs/operations.md`.

For Google API credentials (TTS, STT): use Workload Identity (GKE/Cloud Run) — no static service account keys in the environment. If a key rotation is needed, the Cloud Run service account mapping is updated without touching the binary.

### Task 9.3 — Per-session goroutine isolation

Each session runs in its own goroutine set. A panic in one session's goroutine must be recovered and must not propagate to other sessions or the main goroutine.

All session goroutines are started with a wrapper:

```go
func (s *Session) Go(name string, fn func()) {
    s.wg.Add(1)
    go func() {
        defer s.wg.Done()
        defer func() {
            if r := recover(); r != nil {
                s.log.Error().
                    Str("goroutine", name).
                    Interface("panic", r).
                    Str("stack", string(debug.Stack())).
                    Msg("goroutine panic recovered")
                s.metrics.Incr("gato_goroutine_panics_total")
                s.cancel() // end the session, don't leave it in undefined state
            }
        }()
        fn()
    }()
}
```

This ensures: panic → log → session cancel → goroutine exits → `s.wg.Done()` → session slot freed. The rest of the process continues.

### Task 9.4 — Session-scoped resource limits

Prevent a runaway session from consuming unbounded resources:

**Goroutine cap**: each session should start at most 6 goroutines (audio output, wakeOnCancel, input track, STT result reader, agent func, signal relay). Assert this count in tests. A session that starts > 10 goroutines has a goroutine leak.

**Memory cap**: TTS audio buffers are bounded by the TTS response size. Limit TTS input to `GATO_TTS_MAX_CHARS=1000` characters per Speak call. Return an error for longer inputs — don't OOM.

**AudioQueue cap**: `AudioQueue` with a max depth of 1000 frames (10 seconds of audio). If the queue reaches the cap, log a warning and drop new frames. A queue this full indicates the output goroutine has stalled (possibly due to a Pion WriteSample hang). This is a safety valve, not normal operation.

**STT audio buffer cap**: the 500ms replay buffer is bounded. The VAD accumulation buffer is bounded by the chunk size (512 samples per iteration). Neither grows without bound.

### Task 9.5 — No cross-session data leakage

Verify by code review and tests that:

1. `SileroVAD.Infer()` holds a mutex for the duration of the ONNX call. Session A's `StreamState` is never read or written by Session B's goroutine.

2. `vadState` and `vadContext` are fields on `Session`, not global variables.

3. The `AudioQueue` is per-session (not shared). The `LinearResampler` is per-session (has internal state; sharing it would corrupt audio for both sessions).

4. Transcripts (`heardText`, STT results) are stored only in the session struct, behind a `sync.Mutex`.

5. The Switchboard client's session table is `sync.RWMutex`-protected. No session reads another session's entry without holding a read lock.

Write a test that runs 10 concurrent sessions with distinct audio content and verifies that each session's transcript contains only its own audio content.

### Task 9.6 — Audit logging for session lifecycle

For compliance and debugging, emit an audit log entry at `INFO` level on these events:

```json
{"event": "session.probe",     "session_id": "s_xxx", "node_id": "n_yyy", "deployment_id": "d_zzz", "ts": "..."}
{"event": "session.accepted",  "session_id": "s_xxx", "node_id": "n_yyy"}
{"event": "session.rejected",  "session_id": "s_xxx", "reason": "at_capacity"}
{"event": "session.started",   "session_id": "s_xxx"}
{"event": "session.turn.start","session_id": "s_xxx", "turn_id": 1}
{"event": "session.turn.end",  "session_id": "s_xxx", "turn_id": 1, "transcript_len": 42}
{"event": "session.ended",     "session_id": "s_xxx", "duration_s": 120, "turn_count": 5}
```

`turn_id` is a monotonic integer per session (1, 2, 3, …). It's surfaced in the Agent SDK as `session.TurnID` so customers can correlate their LLM calls with Gato's VAD events.

Audio content is never logged. Transcripts are logged at DEBUG level only, and only if `GATO_LOG_TRANSCRIPTS=true` is set (default false). When false, `transcript_len` (integer character count) is logged instead.

### Task 9.7 — CORS and origin validation

Gato does not serve browser clients directly (the Switchboard handles browser WebSocket connections). However, if Gato ever exposes an HTTP endpoint callable from a browser context, it must validate `Origin` headers.

For now, this means: the Gato HTTP server must not include a wildcard CORS header (`Access-Control-Allow-Origin: *`) unless `DevMode=true`. In production, either no CORS headers (API-only, no browser callers) or explicit `AllowedOrigins` list (same pattern as `switchboard/internal/server/`).

---

## Definition of Done

- [ ] Session A's audio, transcripts, and state cannot be observed from Session B's goroutine (no shared mutable state)
- [ ] Panic in session A's goroutine: session A ends with a log entry; sessions B-Z continue unaffected
- [ ] `GATO_SB_TOKEN` rotation works: deploy new token; old sessions continue; new connections use new token
- [ ] Google credentials come from Workload Identity; no `GOOGLE_APPLICATION_CREDENTIALS` file in the container
- [ ] `AudioQueue` caps at 1000 frames; warns in log; does not OOM
- [ ] Audit log entries emitted on session start/end/turn

---

## Verification

### Unit Tests

- `TestBearerAuth_ConstantTimeCompare`: assert that a wrong token of the same length returns 401; assert `subtle.ConstantTimeCompare` used (not `==`)
- `TestSession_Go_PanicRecovered`: call `session.Go` with a function that panics; assert the panic is logged; assert `session.wg` count reaches 0; assert other sessions unaffected
- `TestAudioQueue_CapDropsFrames`: enqueue 1001 frames; assert queue depth is 1000; assert a warning was logged
- `TestCrossSession_NoSharedState`: create two sessions with the same `SileroVAD`; run concurrent `Infer()` calls; assert each session's `vadState` is independent (inject distinguishable states; verify they don't bleed)

### Integration Tests

- `TestTenantIsolation`: run 5 concurrent sessions; feed each distinct audio (sine waves at different frequencies); assert each session's VAD state and transcript channel contain only its own data
- `TestPanicInSession_OtherSessionsContinue`: run 5 sessions; in session 3, inject a goroutine that panics; assert sessions 1, 2, 4, 5 continue for 10 more seconds; assert `gato_goroutine_panics_total` counter increments
- `TestDrain_ReleasesSessionSlot`: 5 sessions active; kill session 3 via `cancel()`; assert active count drops to 4 within 1s

### E2E

Run the full aiortc E2E test with two simultaneous clients:
1. Client A sends 30 seconds of speech (English)
2. Client B sends 30 seconds of different speech (or silence)
3. Assert Client A hears only its own TTS responses (no cross-contamination)
4. Assert server logs show two distinct `session_id` values throughout
5. Assert `gato_goroutine_panics_total` == 0
