# Gato — Architecture Index

**Gato** (Go + cat, Spanish for cat) is a Go runtime providing audio infrastructure for Voqal Cloud.
A spiritual successor to pipecat, rewritten in Go for resource efficiency and multi-tenancy.

---

## Vision

> *"Resource efficient. Single process, multiple concurrent conversations. Golang handles the audio
> parts — transport, VAD, turn detection, RTVI, user context aggregation — then branches out over
> IPC to customer-provided business logic. Business logic output flows back to Gato for TTS, output
> transport, and assistant aggregation."*

- **Scale to zero**: minimal idle cost; one Go binary per node, many sessions per binary
- **No SFU**: direct 1:1 WebRTC P2P between browser/mobile and bot — proven in
  [voqalcloud SmallWebRTC](~/apps/voqalcloud/agent-sdk/src/voqalcloud/worker/_transport.py)
- **Single UDP port per node**: Pion `UDPMuxDefault` demuxes all ICE sessions by username fragment;
  see [pion ice-single-port example](https://github.com/pion/webrtc/tree/master/examples/ice-single-port)
- **Multi-node via Switchboard**: Gato nodes register with the voqalcloud Switchboard using the
  existing node protocol; session assignment is sticky — the client connects directly to the
  assigned node for the session lifetime, no media relay
- **IPv6 mandatory**: bind to `[::]`, include v6 candidates in ICE
- **TURN**: pluggable ICE server config, not required for hello world

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Voqalcloud Switchboard                                     │
│  session assignment · node registration · signaling relay   │
└────────┬──────────────────┬──────────────────┬─────────────┘
         │                  │                  │
         ▼                  ▼                  ▼
    Gato Node A        Gato Node B        Gato Node C
    (single UDP port, Pion UDPMuxDefault, N sessions each)

         │  WebRTC P2P · Pion · IPv6
         │  (client connects directly to assigned node)
         ▼
┌────────────────────────────────────────────────────────────┐
│  Gato Runtime (Go)                       per-session       │
│                                                            │
│  WebRTC In ─→ VAD ─→ TurnDetect ─→ STT ─→ UserAggregator  │
│               (CGO)    (CGO)                    │          │
│                                                 │          │
│            ┄┄┄┄┄ protobuf frames, bidirectional ┄┄┄┄┄┄    │
│                                                 │          │
│  WebRTC Out ←─ TTS ←─ AssistantAggregator ←────┘          │
│                                                            │
│  Shared: ONNX session pool · HTTP client pool · UDPMux     │
└────────────────────────────────────────────────────────────┘
                       ↕  protobuf / WebSocket
┌────────────────────────────────────────────────────────────┐
│  Business Layer (any language)                             │
│  Owns: LLM · conversation context · tool calls · state     │
│  SDK: mirrors structure of voqalcloud/agent-sdk            │
└────────────────────────────────────────────────────────────┘
```

### Responsibility Split

| Concern                     | Gato (Go)        | Business Layer     |
|-----------------------------|------------------|--------------------|
| WebRTC transport (Pion)     | ✓                |                    |
| VAD — Silero ONNX           | ✓ CGO, in-process|                    |
| Turn detection — TD v3 ONNX | ✓ CGO, in-process|                    |
| STT (provider plugins)      | ✓                |                    |
| User turn aggregation       | ✓ audio→turns    |                    |
| Conversation context        |                  | ✓                  |
| LLM calls                   |                  | ✓                  |
| Tool calls / business logic |                  | ✓                  |
| TTS (provider plugins)      | ✓                |                    |
| Assistant turn aggregation  | ✓                |                    |
| RTVI protocol               | ✓                |                    |

---

## Key Design Decisions

### 1. Frame-Based Pipeline

Gato uses discrete **Frame** objects flowing through **FrameProcessor** chains, intentionally
mirroring pipecat's model ([`frame_processor.py`](~/apps/pipecat-ai/pipecat/src/pipecat/processors/frame_processor.py),
[`frames.py`](~/apps/pipecat-ai/pipecat/src/pipecat/frames/frames.py)):

- **SystemFrames** (interruption, start, end): bypass the data queue, always immediate
- **DataFrames**: flow in order, cancelable by interruption
- **Bidirectional**: downstream (audio→text→IPC) and upstream (IPC→text→audio)
- **Observers**: monitor frame flow without modifying the pipeline

Go mapping: goroutines replace asyncio tasks; buffered channels replace asyncio queues;
`context.Context` cancellation handles in-processor abort. Two goroutines per processor
(one per priority tier) mirror pipecat's system/data queue split.

**Pipecat compatibility goal**: Gato's frame and processor concepts are a deliberate
transliteration of pipecat into Go — not a port, but a parallel implementation of the same
model. New frame types, processor patterns, and pipeline behaviors introduced in pipecat
upstream should map mechanically to Gato equivalents. This keeps the conceptual surface shared
and reduces the cost of evolving both systems in parallel.

**Explicit conversation state**: The pipeline runner maintains an explicit `ConversationState`
enum (IDLE / LISTENING / PROCESSING / SPEAKING). State transitions are driven by SystemFrames
(`InterruptionFrame` → LISTENING, `TurnStartedFrame` → PROCESSING) and are observable via
metrics and logs without needing to inspect frame flow.

### 2. VAD + Turn Detection: CGO/ONNX, In-Process

> *"This is CPU-constrained and must be designed properly."*

- **Silero VAD** runs on every 20ms audio chunk — shared ONNX session, goroutine pool sized to
  `runtime.NumCPU()`. Reference: [livekit-agents vad.py](~/apps/livekit-agents/livekit-agents/livekit/agents/vad.py)
- **Turn Detection v3** (transformer classifier) runs only on VAD→silence transitions, answering
  "is this turn complete?" Reference: [livekit-agents endpointing.py](~/apps/livekit-agents/livekit-agents/livekit/agents/voice/endpointing.py)
- **No sidecar**: inner-loop latency requirement (~20ms chunk budget) excludes subprocess IPC —
  CGO to `onnxruntime` directly. See [~/apps/vad](~/apps/vad) for local ONNX VAD experiments.

### 3. IPC: Protobuf Bidirectional Frames

The Gato ↔ Business Layer boundary is a **stream of length-prefixed protobuf frames** over
WebSocket (or Unix socket for same-host deployment). Each frame carries a type discriminator
and payload — the pipecat frame concept, wire-serialized.

```
Gato → Business:  TurnStarted · TranscriptChunk · TranscriptFinal · TurnInterrupted(heard_text)
Business → Gato:  TextChunk · EndOfTurn · Interrupt
```

Protobuf chosen for: schema enforcement, generated multi-language clients, binary efficiency.
Reference pattern: [voqalcloud switchboard proto](~/apps/voqalcloud/switchboard/internal/proto/).

The proto schema follows the same structural canvas as pipecat's protobuf frame serializer
([`src/pipecat/serializers/`](~/apps/pipecat-ai/pipecat/src/pipecat/serializers/)): a top-level
`Frame` message with a `oneof` payload discriminator, one message type per frame kind. This keeps
the wire format conceptually aligned with the in-process frame model and avoids a separate
serialization vocabulary.

The Business Layer SDK (modeled on [voqalcloud/agent-sdk](~/apps/voqalcloud/agent-sdk/)) owns
conversation context, LLM integration, tool orchestration, and the developer-facing API surface.
Context aggregation lives entirely in the business layer; Gato owns only the audio-to-turns
boundary.

### 4. STT + TTS: Provider Plugins in Go

Concrete implementations for 2–3 providers each, mirroring pipecat's service structure
([`src/pipecat/services/`](~/apps/pipecat-ai/pipecat/src/pipecat/services/)). Go interfaces:

```go
type STTService interface {
    Stream(ctx context.Context, audio <-chan AudioFrame) (<-chan Transcription, error)
}
type TTSService interface {
    Synthesize(ctx context.Context, text string) (<-chan AudioFrame, error)
}
```

Initial providers: **Google Cloud STT** (streaming gRPC) and **Google Cloud TTS** (streaming
synthesis). Chosen for existing API key availability and broad language coverage. Additional
providers follow the same interface and can be added incrementally.

### 5. Interruption Handling

> *"Interruption handling is a key processing — must be handled with care."*

When VAD fires during TTS playback:
1. `InterruptionFrame` (SystemFrame) propagates both directions through the pipeline
2. Each processor's data-channel goroutine is cancelled via `context.Context`
3. Audio output buffer is drained; 20ms chunk size bounds maximum interrupt latency
4. `TurnInterrupted(heard_text)` sent to business layer — `heard_text` is the exact text of
   TTS frames that completed before interruption (not estimated)

Pattern adapted from pipecat-ADK's [HEARD] mechanism:
[`interruption.py`](~/apps/pipecat-adk/src/pipecat_adk/interruption.py)

### 6. Multi-Node Deployment

Gato integrates with the voqalcloud Switchboard using the existing
[node protocol](~/apps/voqalcloud/switchboard/internal/proto/) (protobuf over WebSocket):
register → receive session probes → ack/nack → relay signaling → media P2P.

Session assignment is made once at creation time and is sticky for the session lifetime.
The client connects directly to the assigned Gato node's IP; there is no relay in the media
path. Each node exposes one UDP port; Pion `UDPMuxDefault` demuxes all sessions on that port
by ICE username fragment. Horizontal scale = more Gato nodes registered with the Switchboard.

Gato nodes replace the Python `agent-sdk` workers; the Switchboard is unchanged.

### 7. Session Worker Pattern

One goroutine cluster per session, sharing process-level resources. Reference:
[livekit-server `rtcSessionWorker`](~/apps/livekit/pkg/rtc/room.go). Shared resources:
ONNX runtime sessions (thread-safe for inference), HTTP client pools, UDPMux.

### 8. Voqalcloud Feature Scope

Gato's initial scope covers the core conversation loop. Two voqalcloud features are deferred
to a later phase after the concept is proven:

- **Recording**: dual-leg audio capture (WebM/Opus) as done in
  [`_recording.py`](~/apps/voqalcloud/agent-sdk/src/voqalcloud/worker/_recording.py) — phase 2.
- **RTVI data channel**: voqalcloud console uses RTVI-formatted messages over the WebRTC data
  channel ([`VoqalWebRTCTransport`](~/apps/voqalcloud/console/src/lib/voqal-webrtc-transport.ts));
  required for the existing client SDK to work unchanged — phase 2.

Basic session metrics (duration, latency, error rates) are emitted from day 1 via the observer
layer; the recording and RTVI protocol surface are not.

---

## Reference Implementations

| Concept                          | Reference                                                                                     |
|----------------------------------|-----------------------------------------------------------------------------------------------|
| Frame / processor model          | [pipecat frame_processor.py](~/apps/pipecat-ai/pipecat/src/pipecat/processors/frame_processor.py) |
| P2P WebRTC + signaling           | [voqalcloud transport.py](~/apps/voqalcloud/agent-sdk/src/voqalcloud/worker/_transport.py)   |
| Protobuf IPC protocol            | [voqalcloud switchboard proto](~/apps/voqalcloud/switchboard/internal/proto/)                  |
| Business-layer SDK               | [voqalcloud agent-sdk](~/apps/voqalcloud/agent-sdk/)                                          |
| Session worker goroutine         | [livekit-server room.go](~/apps/livekit/pkg/rtc/room.go)                                      |
| UDP mux (single port)            | [livekit roommanager.go](~/apps/livekit/pkg/service/roommanager.go)                           |
| Multi-node switchboard protocol  | [voqalcloud node proto](~/apps/voqalcloud/switchboard/internal/proto/)                         |
| VAD integration pattern          | [livekit-agents vad.py](~/apps/livekit-agents/livekit-agents/livekit/agents/vad.py)           |
| Turn / endpointing               | [livekit-agents endpointing.py](~/apps/livekit-agents/livekit-agents/livekit/agents/voice/endpointing.py) |
| STT / TTS plugin pattern         | [pipecat services/](~/apps/pipecat-ai/pipecat/src/pipecat/services/)                         |
| [HEARD] interruption pattern     | [pipecat-adk interruption.py](~/apps/pipecat-adk/src/pipecat_adk/interruption.py)            |

---

## Open Questions

All architectural questions are settled. See [`open-questions.md`](./open-questions.md).
