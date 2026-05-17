# Gato — Open Architectural Questions

Track and resolve these before or during hello-world implementation.
Status: [ ] open  [~] discussed, pending  [x] resolved

---

## Q1: Voqalcloud Day-1 Feature Requirements [ ]

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

## Q2: STT/TTS Providers for Hello World [ ]

**Question:** Which STT and TTS providers are the hello-world targets?

**Candidates:**
- STT: Deepgram (streaming WebSocket, low latency) · Google Cloud STT · AssemblyAI
- TTS: ElevenLabs (streaming) · Google Cloud TTS · Cartesia · Azure TTS

**Decision needed:** Pick 1 STT + 1 TTS to implement first.

---

## Q3: CGO Build Pipeline [ ]

**Question:** CGO adds build complexity. What's the strategy?

- `onnxruntime-go` wrapper for ONNX runtime
- Requires `libonnxruntime.so` or static linking
- Cross-compilation is harder with CGO
- Docker build with pinned ONNX runtime version is the practical path

**Decision needed:** Accept CGO complexity from day 1, or stub VAD/TD for hello world and
add CGO in phase 2?

---

## Q4: Protobuf Schema Versioning [ ]

**Question:** Should the Gato ↔ Business Layer proto schema be versioned from day 1?

Proto files evolve; field additions are backward compatible, but type changes break.
Define a `version` field in the handshake message from the start.

**Decision needed:** Version field in proto from the start? Who owns the proto repo?

---

## Summary Table

| # | Question                                | Status | Priority |
|---|-----------------------------------------|--------|----------|
| 1 | Voqalcloud day-1 feature requirements   | [ ]    | High     |
| 2 | STT/TTS providers for hello world       | [ ]    | High     |
| 3 | CGO build pipeline                      | [ ]    | Medium   |
| 4 | Proto schema versioning                 | [ ]    | Low      |
