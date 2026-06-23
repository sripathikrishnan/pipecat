# Epic 08: Observability & SRE

## Business Meaning

Operators need to know if Gato is healthy, how many sessions are active, where latency comes from, and how to safely deploy new versions without dropping calls. This epic wires up the logging, metrics, health endpoints, and graceful drain that make Gato a trustworthy production service.

Without this epic, Gato is a black box. With it, an on-call engineer can diagnose a problem, a load balancer can route around a bad instance, and a deployment can roll out without impacting active calls.

---

## Background

Follow the exact patterns established in `switchboard/`:
- **Zerolog**: context-injected logger; `logctx.Get(ctx)` returns the session's logger. Zero package-level loggers. See `switchboard/internal/logctx/`.
- **Prometheus**: fresh registry per `NewMetrics()` call. Named counters, gauges, histograms. See `switchboard/internal/metrics/metrics.go`.
- **Shutdown**: SIGTERM → `/ready` 503 → drain → force-close → exit 0. See `switchboard/internal/shutdown/`.
- **Config**: all log/metrics settings come from env vars. See `switchboard/internal/config/`.

---

## Tasks

### Task 8.1 — Structured logging with Zerolog

Create `gato/internal/logctx/logctx.go`. Copy verbatim from `switchboard/internal/logctx/` — same pattern, different module path.

```go
func NewLogger(level, format string, noColor bool) zerolog.Logger
func With(ctx context.Context, l zerolog.Logger) context.Context
func Get(ctx context.Context) zerolog.Logger
```

Every goroutine that has access to a session must carry the session logger in its context:

```go
log := logctx.Get(ctx).With().Str("session_id", string(s.ID)).Logger()
ctx = logctx.With(ctx, log)
```

Derive child loggers at subsystem boundaries. Never use `log.Print` or `fmt.Println`. Secrets (tokens, session payloads) are never logged — use `"***"` placeholder.

**Log levels:**
- `DEBUG`: per-frame events (VAD probabilities, ICE candidate exchange), 1% sampled in production
- `INFO`: session lifecycle events (probe, ack, start, end), startup, shutdown
- `WARN`: recoverable errors (STT reconnect, TTS timeout retry), customer agent panic
- `ERROR`: non-recoverable errors (ONNX load failure, Switchboard auth failure)

Structured fields that must appear on every session-scoped log entry:
```json
{"session_id": "s_xxx", "deployment_id": "d_yyy", "node_id": "n_zzz"}
```

### Task 8.2 — Prometheus metrics

Create `gato/internal/metrics/metrics.go`. Follow the switchboard pattern: `NewMetrics()` returns a struct with all instruments; mount at `GET /metrics`.

```go
type Metrics struct {
    // Session lifecycle
    SessionsActive    prometheus.Gauge      // by state: probing, negotiating, live
    SessionsTotal     prometheus.Counter    // by outcome: completed, failed, terminated

    // VAD
    VADInferDuration  prometheus.Histogram  // per-call inference time in seconds

    // STT
    STTTranscriptLatency prometheus.Histogram  // TurnEnd → final transcript
    STTStreamReconnects  prometheus.Counter
    STTErrors           prometheus.Counter     // by gRPC status code

    // TTS
    TTSLatency          prometheus.Histogram  // Speak() call → first audio frame
    TTSErrors           prometheus.Counter

    // Audio output
    AudioInterrupts     prometheus.Counter
    AudioQueueDepth     prometheus.Gauge

    // Switchboard
    SwitchboardReconnects prometheus.Counter
    ProbesReceived        prometheus.Counter
    ProbesAcked           prometheus.Counter
    ProbesNacked          prometheus.Counter

    // Process
    ActiveSessions      prometheus.Gauge    // total across all states
}
```

All histogram buckets must be hand-tuned to the expected range:
- `VADInferDuration`: `{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1}` seconds
- `STTTranscriptLatency`: `{0.1, 0.3, 0.5, 1.0, 2.0, 5.0}` seconds
- `TTSLatency`: `{0.2, 0.5, 1.0, 2.0, 5.0}` seconds

Mount at `GET /metrics` using `promhttp.HandlerFor(registry, promhttp.HandlerOpts{})`.

### Task 8.3 — Health and readiness endpoints

Create `gato/internal/server/health.go`.

`GET /health` — always returns 200 with a JSON body while the process is alive:
```json
{"status": "ok", "node_id": "gato-001", "sessions": 5, "version": "1.2.3"}
```

`GET /ready` — returns 200 when Gato is accepting sessions; returns 503 during drain:
```json
{"status": "draining", "active_sessions": 3}
```

Load balancers poll `/ready`. Cloud Run's startup probe uses `/health`.

### Task 8.4 — Graceful SIGTERM drain

Create `gato/internal/shutdown/coordinator.go`. Port from `switchboard/internal/shutdown/` with adjustments for Gato's session lifecycle.

Drain sequence triggered by SIGTERM or SIGINT:

1. `coordinator.BeginDrain()`:
   - Set `draining = true` atomically
   - Switchboard client: `accepting_jobs = false` in next ping
   - `/ready` returns 503 immediately

2. Wait for `activeSessionCount == 0` with timeout `T_SHUTDOWN` (default 8s)

3. On timeout: force-cancel all remaining sessions via `session.cancel()`

4. Wait for all session goroutines (`globalWG.Wait()`)

5. Flush pending control plane events

6. Close Switchboard WebSocket (sends close frame)

7. Exit 0

The shutdown coordinator must not block the SIGTERM handler — it runs in a goroutine and signals completion via a channel. `main.go` blocks on that channel.

### Task 8.5 — pprof and debug endpoints

Mount Go's built-in pprof at `GET /debug/pprof/*` (net/http/pprof). This must be on a separate internal port (e.g., `:8391`) that is not exposed publicly.

Enable `runtime.SetMutexProfileFraction(5)` to capture mutex contention (useful for diagnosing VAD lock contention at high session counts).

The EXP-009 performance harness used `runtime.NumGoroutine()` to verify zero goroutine growth. In production, expose this as a gauge: `gato_goroutines` — alert if it grows > 50 per session.

### Task 8.6 — Control plane session events

When significant session state changes occur, post events to the control plane (same pattern as the switchboard events package — `switchboard/internal/events/`):

Events to post:
- `session.started` (session_id, deployment_id, node_id, timestamp)
- `session.ended` (session_id, duration_seconds, turn_count, outcome: completed|failed|terminated)
- `session.interrupted` (session_id, heard_text, turn_id)

Post asynchronously via a buffered event queue; batch dispatch on a 500ms timer or when the queue reaches 100 events. On dispatch failure (CP unreachable), log at warn and retry with exponential backoff (cap 30s). Drop events after 3 retries and increment a `gato_events_dropped_total` counter.

Do not let event dispatch block the session goroutine.

### Task 8.7 — Startup validation and crash-fast config

In `main.go`, before opening any listeners, validate:
1. ONNX model file exists and loads successfully (`NewSileroVAD(cfg.VADModelPath)`)
2. Google credentials are available (`google.FindDefaultCredentials(ctx, ...scopes)`)
3. `GATO_SB_URL` is reachable (TCP dial within 5s)
4. All required config fields are set (`cfg.Validate()`)

If any validation fails, log the reason at ERROR level and exit with code 1. Do not start listening for traffic.

Log all config values at INFO level at startup (redacting secret values). This is critical for debugging production issues where the deployed config differs from expectations.

---

## Definition of Done

- [ ] Every log line for a session contains `session_id`, `deployment_id`, `node_id` as structured fields
- [ ] `GET /metrics` returns valid Prometheus text exposition with all named metrics
- [ ] `GET /health` returns 200; `GET /ready` returns 503 after SIGTERM
- [ ] SIGTERM: in-flight sessions complete; process exits 0 within `T_SHUTDOWN + 2s`
- [ ] `GET /debug/pprof/goroutine` shows no leaked goroutines after 5 sessions complete
- [ ] Startup fails fast (exit 1) if the ONNX model file is missing
- [ ] No secret values appear in any log line

---

## Verification

### Unit Tests

- `TestLogctx_Get_ReturnsInjected`: inject a logger; assert `logctx.Get(ctx)` returns it; assert `logctx.Get(context.Background())` returns a no-op logger
- `TestMetrics_AllInstrumentsRegistered`: create `NewMetrics()`; assert all named metrics are registered in the registry (no panics, no duplicates)
- `TestReadiness_503OnDrain`: create handler; call `BeginDrain()`; assert `GET /ready` returns 503
- `TestDrain_WaitsForSessions`: start a fake session that sleeps 500ms; call drain; assert drain completes within 600ms (not immediately)
- `TestDrain_ForcesCancelOnTimeout`: start a fake session that sleeps 30s; set `T_SHUTDOWN=1s`; assert drain force-cancels and exits within 2s

### Integration Tests

- `TestStartup_ValidatesONNX`: set `GATO_VAD_MODEL_PATH=/nonexistent`; assert process exits with code 1 within 5s
- `TestStartup_ValidatesConfig`: unset `GATO_SB_URL`; assert `config.Validate()` returns error
- `TestGracefulDrain_E2E`: start Gato; send a probe; start a 5-second session; send SIGTERM at t=1s; assert session completes at t=5s; assert process exits at t=5s (not t=1s, not t=9s)
- `TestMetrics_SessionCounting`: start 5 sessions; assert `gato_sessions_active == 5`; end 2; assert `== 3`

### Server E2E

Deploy Gato to Cloud Run staging. Run 10-minute soak test with 5 concurrent sessions. Assert:
- `gato_goroutines` gauge is stable (not growing)
- `gato_vad_infer_duration_seconds` p99 < 1ms
- Zero `gato_sessions_total{outcome="failed"}` (all sessions complete cleanly)
- Grafana dashboard shows all panels green
