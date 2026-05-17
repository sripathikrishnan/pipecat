# Gato — Open Architectural Questions

Track and resolve these before or during hello-world implementation.
Status: [ ] open  [~] discussed, pending  [x] resolved

---

## Q1: Frame Model vs Event Streaming [ ]

**Your question:** Pipecat frames/frameprocessors (well understood) vs LiveKit Agents' event
streaming (not understood) — pros, cons, which for Gato?

**LiveKit Agents event model explained:**
LiveKit Agents (`agent_session.py`) does NOT have a pipeline at all. There is a single
`AgentSession` object that owns all state. Components (VAD, STT, LLM, TTS) are wired to it
via dependency injection. The session emits named async events — `user_state_changed`,
`agent_state_changed`, `conversation_item_added`, `function_tools_executed` — and handlers
subscribe to these. Audio flows through `AudioInput`/`AudioOutput` I/O objects, not frames.
The STT→LLM→TTS chain is an *implicit* state machine inside `AgentSession`, not an explicit
pipeline.

**Comparison:**

| Dimension            | Pipecat Frames                      | LiveKit Event Streaming              |
|----------------------|-------------------------------------|--------------------------------------|
| Control flow         | Explicit — frames carry intent       | Implicit — state machine in session  |
| Interruption         | InterruptionFrame floods pipeline   | Agent state machine handles it       |
| Backpressure         | Channel buffering per processor     | Hidden inside session internals      |
| Introspection        | Observers see every frame           | Only emitted events visible          |
| Composability        | Add processors anywhere in chain    | Must subclass or hook session        |
| Testability          | Feed frames in, assert frames out   | Mock events, harder to isolate       |
| Verbosity            | High — type switch per frame type   | Low — just subscribe to events       |
| Multi-session        | Natural — one pipeline per session  | Natural — one session object         |

**Recommendation for Gato:** Frame model. We have bidirectional IPC frames that *must* be
first-class objects. Interruption correctness requires explicit propagation semantics.
Observers are required for Voqal Cloud telemetry. The verbosity cost is worth it.

**Decision needed:** Confirm frame model, or counter-argument?

---

## Q2: Voqalcloud Day-1 Feature Requirements [ ]

**Your question:** What voqalcloud APIs/features must Gato expose from day 1?

The integration model is settled (Gato registers with Switchboard via the existing node
protocol; see `index.md`). The open question is scope:

- **Recording**: voqalcloud records both audio legs as WebM/Opus
  ([`_recording.py`](~/apps/voqalcloud/agent-sdk/src/voqalcloud/worker/_recording.py)).
  Required for day 1, or phase 2?
- **RTVI on data channel**: voqalcloud console expects RTVI-formatted messages
  ([`VoqalWebRTCTransport`](~/apps/voqalcloud/console/src/lib/voqal-webrtc-transport.ts)).
  RTVI-compatible = existing client SDK works unchanged. Required for day 1?
- **Metrics / observability**: session duration, latency, error rates. Day 1 or phase 2?

**Decision needed:** Which of these are required before Gato can replace agent-sdk in production?

---

## Q3: STT/TTS Providers for Hello World [ ]

**Question:** Which STT and TTS providers are the hello-world targets?

**Candidates:**
- STT: Deepgram (streaming WebSocket, low latency) · Google Cloud STT · AssemblyAI
- TTS: ElevenLabs (streaming) · Google Cloud TTS · Cartesia · Azure TTS

**Decision needed:** Pick 1 STT + 1 TTS to implement first.

---

## Q4: CGO Build Pipeline [ ]

**Question:** CGO adds build complexity. What's the strategy?

- `onnxruntime-go` wrapper for ONNX runtime
- Requires `libonnxruntime.so` or static linking
- Cross-compilation is harder with CGO
- Docker build with pinned ONNX runtime version is the practical path

**Decision needed:** Accept CGO complexity from day 1, or stub VAD/TD for hello world and
add CGO in phase 2?

---

## Q5: Protobuf Schema Versioning [ ]

**Question:** Should the Gato ↔ Business Layer proto schema be versioned from day 1?

Proto files evolve; field additions are backward compatible, but type changes break.
Define a `version` field in the handshake message from the start.

**Decision needed:** Version field in proto from the start? Who owns the proto repo?

---

## Summary Table

| # | Question                                | Status | Priority |
|---|-----------------------------------------|--------|----------|
| 1 | Frame model vs event streaming          | [ ]    | High     |
| 2 | Voqalcloud day-1 feature requirements   | [ ]    | High     |
| 3 | STT/TTS providers for hello world       | [ ]    | High     |
| 4 | CGO build pipeline                      | [ ]    | Medium   |
| 5 | Proto schema versioning                 | [ ]    | Low      |
