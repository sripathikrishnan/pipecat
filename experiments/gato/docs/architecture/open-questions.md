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

## Q2: Load Balancing Across Multiple Gato Nodes [ ]

**Your question:** Single binary, single UDP port — how does load balancing work across nodes?

**The problem:** A WebRTC P2P session requires all ICE packets to reach the *same* process.
UDP is connectionless — a load balancer can't hash UDP packets to a backend by connection
(no TCP session). Once ICE is established, packets flow directly P2P (no server relay needed),
but *signaling* must reach the right node.

**Options:**

**Option A: DNS round-robin + sticky signaling**
- Each node registers its own IP/hostname in DNS
- Client connects signaling WebSocket to a specific node (via URL from control plane)
- Control plane assigns client→node mapping at session creation
- No L4 load balancer in the media path at all (P2P skips it anyway)
- Works today with voqalcloud's control plane model

**Option B: Voqalcloud Switchboard as the router**
- Switchboard (existing Go binary) handles load balancing + session assignment
- Gato nodes register with switchboard like voqalcloud nodes do today
- Switchboard relays signaling to the right Gato node
- Reuse the entire [voqalcloud node protocol](~/apps/voqalcloud/switchboard/internal/proto/)
- Gato binary replaces `agent-sdk` child process; switchboard stays unchanged
- **This is the natural integration path with voqalcloud** (see Q4)

**Option C: L7 WebSocket proxy (e.g., Nginx/Caddy/Envoy)**
- Proxy routes WebSocket signaling by session ID in URL path
- Simple operationally, but adds a hop and a dependency

**Recommendation:** Option B. It reuses proven infrastructure, solves the routing problem,
and is the right integration path into voqalcloud anyway.

**Decision needed:** Confirm Option B, or discuss constraints?

---

## Q3: Integration with Voqalcloud [ ]

**Your question:** How does Gato fit into the existing voqalcloud platform?

**Current voqalcloud architecture:**
```
Browser → Switchboard (Go) → Agent Node (Python, voqalcloud agent-sdk)
                                   ↓
                           Child process (pipecat bot)
```

**Proposed Gato integration:**
```
Browser → Switchboard (Go) → Gato Node (Go)
                                   ↓ protobuf frames
                           Business Logic (any lang, Gato SDK)
```

Gato replaces the `agent-sdk` + pipecat stack. The switchboard remains unchanged.
Gato registers with switchboard using the same node protocol (protobuf over WebSocket).
Session lifecycle: probe → ack → signal relay → ready → media P2P.

**Migration path:**
1. Gato implements the [switchboard node protocol](~/apps/voqalcloud/switchboard/internal/proto/)
2. Voqalcloud control plane dispatches to Gato nodes (same `deployment_id` routing)
3. Existing pipecat-based bots continue working via old agent-sdk nodes
4. New bots built with Gato SDK

**Key difference:** voqalcloud agent-sdk creates a child *process* per session.
Gato runs all sessions in one process — the switchboard doesn't care, it only sees
the node registration (max_sessions, active_sessions).

**Decision needed:** Confirm this integration model. What voqalcloud APIs/features must
Gato expose from day 1 (recording? RTVI? metrics)?

---

## Q4: RTVI Compatibility [ ]

**Question (ours):** Should the WebRTC data channel protocol be RTVI-compatible?

**Context:** RTVI (Real-Time Voice Interface) is pipecat's client protocol for data channel
messages. The pipecat LiveKit transport uses it. Voqalcloud console uses it via
[`VoqalWebRTCTransport`](~/apps/voqalcloud/console/src/lib/voqal-webrtc-transport.ts).

**Tradeoffs:**
- RTVI compatibility = existing voqalcloud client SDK works with Gato with zero changes
- RTVI is JSON-based, designed for pipecat — may not map cleanly to Gato's frame model
- We control both ends; can evolve protocol independently if we break compatibility

**Decision needed:** RTVI-compatible data channel, or new protocol?

---

## Q5: STT/TTS Providers for Hello World [ ]

**Question (ours):** Which STT and TTS providers are the hello-world targets?

**Candidates:**
- STT: Deepgram (streaming WebSocket, low latency) · Google Cloud STT · AssemblyAI
- TTS: ElevenLabs (streaming) · Google Cloud TTS · Cartesia · Azure TTS

**Decision needed:** Pick 1 STT + 1 TTS to implement first.

---

## Q6: Recording in Gato [ ]

**Question (ours):** Recording must be handled. Where and how?

**Voqalcloud approach:** Intercepts encoded Opus frames from aiortc's decoder queue
([`_recording.py`](~/apps/voqalcloud/agent-sdk/src/voqalcloud/worker/_recording.py)).
Writes WebM via PyAV. Stores locally or GCS.

**Gato approach options:**
- A: Intercept raw PCM in the pipeline (before/after WebRTC codec) — simpler, bigger files
- B: Intercept encoded Opus from Pion's RTP track — same as voqalcloud, compressed

**Decision needed:** Required for hello world, or phase 2?

---

## Q7: CGO Build Pipeline [ ]

**Question (ours):** CGO adds build complexity. What's the strategy?

- `onnxruntime-go` wrapper for ONNX runtime
- Requires `libonnxruntime.so` or static linking
- Cross-compilation is harder with CGO
- Docker build with pinned ONNX runtime version is the practical path

**Decision needed:** Accept CGO complexity from day 1, or stub VAD/TD for hello world and
add CGO in phase 2?

---

## Q8: Protobuf Schema Versioning [ ]

**Question (ours):** Should the Gato ↔ Business Layer proto schema be versioned from day 1?

Proto files evolve; field additions are backward compatible, but type changes break.
Define a `version` field in the handshake message from the start.

**Decision needed:** Version field in proto from the start? Who owns the proto repo?

---

## Summary Table

| # | Question                                | Status | Priority |
|---|-----------------------------------------|--------|----------|
| 1 | Frame model vs event streaming          | [ ]    | High     |
| 2 | Load balancing across nodes             | [ ]    | High     |
| 3 | Voqalcloud integration model            | [ ]    | High     |
| 4 | RTVI compatibility                      | [ ]    | Medium   |
| 5 | STT/TTS providers for hello world       | [ ]    | High     |
| 6 | Recording                               | [ ]    | Medium   |
| 7 | CGO build pipeline                      | [ ]    | Medium   |
| 8 | Proto schema versioning                 | [ ]    | Low      |
