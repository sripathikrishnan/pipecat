# Gato Production Plan

Gato is a Go-based voice agent runtime for Voqalcloud. It replaces the Python `agent-sdk` worker model (one subprocess per session) with a single Go process that serves N concurrent sessions. All risk validation is complete across 10 experiments (see `experiments/gato/experiments/`).

This plan targets the **voqalcloud monorepo** (`github.com/recruit41/voqalcloud`). Gato lives at `gato/` as a first-class Go module alongside `switchboard/`.

---

## What Gato Does (Business Level)

A customer deploys Gato to Voqalcloud. When a user calls in, Voqalcloud's Switchboard assigns that session to a Gato node. Gato:

1. Accepts the WebRTC audio stream from the browser
2. Detects when the user starts and stops speaking (VAD + turn detection)
3. Transcribes the speech (Google STT)
4. Invokes the customer's business logic (LLM, tools, etc.) via the Go Agent SDK
5. Synthesizes a response (TTS), encodes it as Opus, and streams it back
6. Handles interruptions: if the user speaks while Gato is talking, Gato stops mid-utterance and remembers exactly what was heard

All of this happens in real time, with sub-100ms interrupt latency, at 10+ concurrent sessions per process.

---

## Epics

| # | Epic | Business Meaning | Status |
|---|------|-----------------|--------|
| 01 | [Switchboard Integration](epic-01-switchboard-integration.md) | Gato connects to the Voqalcloud platform and accepts sessions | [ ] |
| 02 | [WebRTC Audio I/O](epic-02-webrtc-audio.md) | Real-time voice flows in from the browser and back out | [ ] |
| 03 | [VAD & Turn Detection](epic-03-vad-turn-detection.md) | Gato knows when the user starts and stops speaking | [ ] |
| 04 | [Speech Transcription](epic-04-stt-integration.md) | User speech is converted to text reliably | [ ] |
| 05 | [Go Agent SDK](epic-05-agent-sdk-go.md) | Customers write business logic in Go, with a clean API | [ ] |
| 06 | [Speech Synthesis & Output](epic-06-tts-output.md) | Gato speaks back in real time, interruptibly | [ ] |
| 07 | [Pipecat Frame Pipeline](epic-07-pipecat-compatibility.md) | Core pipeline is compatible with Pipecat's frame model | [ ] |
| 08 | [Observability & SRE](epic-08-observability-sre.md) | Operators can monitor, alert, drain, and deploy safely | [ ] |
| 09 | [Security & Multi-tenancy](epic-09-security-multitenancy.md) | Sessions are isolated; secrets are managed safely | [ ] |
| 10 | [Deployment & CI/CD](epic-10-deployment-cicd.md) | Gato ships on Cloud Run, tested under load | [ ] |

---

## Dependency Order

Epics are ordered by dependency. You can start 01–07 in parallel after setting up the module skeleton, but 08–10 touch everything and should come last.

```
01 (Switchboard) ──┐
02 (WebRTC)        ├──► 07 (Pipeline) ──► 08 (SRE) ──► 10 (Deploy)
03 (VAD)           │
04 (STT)           ├──► 06 (TTS) ──────► 09 (Security)
05 (Agent SDK)  ───┘
```

---

## Key Existing References

| Reference | Location | Relevance |
|-----------|----------|-----------|
| Experiments 001–010 | `experiments/gato/experiments/` | All architectural decisions proven |
| `LEARNINGS.md` | `experiments/gato/experiments/LEARNINGS.md` | Consolidated findings per experiment |
| EXP-008 pipeline | `experiments/gato/experiments/exp-008/` | Full pipeline: VAD+STT+TTS+WebRTC |
| EXP-009 perf | `experiments/gato/experiments/exp-009/` | 10-session load, goroutine safety |
| EXP-010 E2E | `experiments/gato/experiments/exp-010/` | Real WebRTC client proving end-to-end |
| Switchboard CLAUDE.md | `switchboard/CLAUDE.md` | Go patterns: state machines, logging, testing |
| Switchboard node handler | `switchboard/internal/node/handler.go` | Exact node handshake protocol |
| Switchboard proto | `switchboard/proto/switchboard.proto` | Wire protocol messages |
| Agent SDK | `agent-sdk/src/voqalcloud/worker/` | Python reference for Go SDK design |
| Switchboard config | `switchboard/internal/config/config.go` | Env-var config pattern |
| Switchboard metrics | `switchboard/internal/metrics/metrics.go` | Prometheus instrument patterns |
| Switchboard Dockerfile | `switchboard/Dockerfile` | Multi-stage build reference |

---

## Module Layout (Target)

```
voqalcloud/
└── gato/
    ├── cmd/
    │   └── gato/
    │       └── main.go              # entrypoint: config → tables → HTTP server
    ├── internal/
    │   ├── audio/                   # Resampler, AudioQueue, VAD wrapper
    │   ├── codec/                   # Opus encoder/decoder wrappers
    │   ├── config/                  # Env-var config (mirrors switchboard pattern)
    │   ├── logctx/                  # Zerolog context injection (copy from switchboard)
    │   ├── metrics/                 # Prometheus instruments
    │   ├── pipeline/                # FrameProcessor, Pipeline, priority queue
    │   ├── proto/                   # switchboard.pb.go (shared/symlinked)
    │   ├── server/                  # HTTP server, routes
    │   ├── session/                 # Session lifecycle, goroutine tracking
    │   ├── shutdown/                # Drain coordinator (copy from switchboard)
    │   ├── state/                   # SessionTable, named ID types
    │   ├── stt/                     # Google STT gRPC streaming
    │   ├── switchboard/             # Switchboard WebSocket client
    │   ├── tts/                     # TTS client interface + Google impl
    │   ├── vad/                     # Silero VAD v5 ONNX
    │   └── webrtc/                  # Pion session setup
    ├── sdk/                         # Public Go Agent SDK (customer-facing)
    │   └── voqal/
    │       ├── agent.go             # AgentFunc type, SessionContext
    │       ├── pipeline.go          # FrameProcessor, Frame types
    │       └── tts.go               # TTSClient interface
    ├── tests/
    │   ├── integration/             # Real Switchboard fake, real WebRTC
    │   └── e2e/                     # aiortc Python client (from exp-010)
    ├── Dockerfile
    ├── Makefile
    ├── go.mod
    └── CLAUDE.md                    # Implementation notes for this service
```

---

## Non-Functional Requirements

| Requirement | Target | Verification |
|-------------|--------|-------------|
| Sessions per process | ≥ 10 concurrent | EXP-009 harness at 10 sessions |
| VAD latency p99 | < 1ms per chunk | EXP-009: 0.55ms at N=10 |
| Interrupt latency | < 50ms | EXP-001: 2ms measured |
| Goroutine leak | Zero growth | EXP-009: `runtime.NumGoroutine()` stable |
| SIGTERM drain | Completes in < 30s | Integration test with active sessions |
| Memory per session | < 50MB | pprof heap snapshot at N=10 |
| Log verbosity | Structured JSON, no secrets | Zerolog + logctx pattern |
| Metrics | Prometheus scrape at /metrics | Grafana dashboard |
