# EXP-008: Hello World Session

**Risk addressed**: Does the full pipeline work end-to-end? Can a user speak into a browser,
have it transcribed, processed by a stub business layer, and hear a TTS response — with
interruption working correctly?

**Status**: [ ]

**Depends on**: EXP-003, EXP-004, EXP-005, EXP-006, EXP-007

---

## Hypothesis

All prior experiments tested components in isolation. This experiment connects them into the
actual pipeline from `bot.go`. Each processor is the real implementation, not a stub.

The one exception: the business layer is a stub (an in-process Go function that echoes
"You said: [transcript]" as the LLM response). The IPC protobuf bridge is real but connects
to this local stub, not an external service.

If this works, the architecture is validated. The business layer stub can be replaced by a
real Python business layer connected over the WebSocket IPC.

---

## Pipeline

```
Browser mic → [Pion input] → [Silero VAD] → [TurnDetectorV3] → [Google STT]
    → [UserAggregator] → [IPC stub] → [AssistantAggregator]
    → [Google TTS] → [Pion output] → Browser speaker
```

IPC stub (in-process, bypasses protobuf):
```go
func stubBusinessLayer(transcript string) string {
    return "You said: " + transcript
}
```

---

## Program

```
experiments/gato/experiments/exp-008/
  main.go         — HTTP server, Pion setup, full pipeline
  pipeline.go     — assembles the pipeline from real components
  ipc_stub.go     — in-process stub implementing the IPC interface
  index.html      — browser test page (mic capture, speaker output, interrupt button)
  testdata/       — ONNX models (symlinks to exp-004/testdata)
```

---

## Test Scenarios

### Scenario 1 — Basic conversation

1. Open browser, connect WebRTC.
2. Say "Hello, how are you today?"
3. Wait for bot response.
4. **Assert**:
   - STT transcript appears in browser logs within 2 seconds of speech end.
   - TTS audio plays within 1 second of transcript appearing.
   - Heard audio: "You said: Hello, how are you today?"

### Scenario 2 — Interruption

1. Say a long sentence: "Tell me about the history of ancient Rome and the fall of the empire"
2. While the bot is responding, say "Stop"
3. **Assert**:
   - Bot audio stops within 100 ms of "Stop" detected by VAD.
   - Browser logs show `TurnInterrupted(heard_text=...)` where `heard_text` is a valid
     prefix of the bot's response.
   - Bot then processes "Stop" as a new turn and responds.

### Scenario 3 — Multiple turns

1. Complete 5 conversational turns without disconnecting.
2. **Assert**:
   - No goroutine leak between turns (`runtime.NumGoroutine()` stable).
   - Memory does not grow unboundedly (profile with `pprof`).
   - Each turn has correct TTFT (STT) and TTFA (TTS audio) < 2 seconds.

---

## Success Criteria

1. **Scenario 1**: correct transcript + TTS audio plays within 2 seconds.
2. **Scenario 2**: interruption stops bot audio within 100 ms; subsequent turn processed.
3. **Scenario 3**: 5 turns complete; no goroutine leak; memory stable.
4. Process does not crash or panic on any scenario.

---

## Measurements to Record

For each of 5 turns:
- TTFT: time from end-of-user-speech to first STT interim result
- TTFA: time from STT final result to first audio byte played in browser
- Interrupt latency (scenario 2): time from VAD speech-start to bot audio stop

Also record:
- `runtime.NumGoroutine()` at start, after turn 1, after turn 5
- RSS memory at start and after turn 5
- Any errors or unexpected frame types logged by the pipeline

---

## What Failure Looks Like

- No audio plays → output transport pacing broken (re-check EXP-001/EXP-002).
- VAD never fires → check PCM format from Pion input track (ensure it's s16le, 16 kHz, mono).
- STT never produces final results → check stream management from EXP-006.
- Bot does not stop on interrupt → check InterruptionFrame routing through pipeline.
- Goroutine leak after 5 turns → a goroutine in one of the processors is not cleaning up on
  context cancellation. Run with `-race` and `pprof` goroutine dump.

---

## Promotion Criteria

When EXP-008 passes, the Gato architecture is validated. The next step is:
1. Remove the IPC stub and connect a real Python business layer over the protobuf WebSocket.
2. Add Silero VAD turn detection (replacing the TurnDetectorV3 stub if it was stubbed).
3. Register with the voqalcloud Switchboard for end-to-end cloud testing.
