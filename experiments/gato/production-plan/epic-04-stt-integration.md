# Epic 04: Speech Transcription (STT)

## Business Meaning

The user's words must become text before the LLM can respond. This epic wires up Google Cloud Speech-to-Text as a real-time streaming transcription service. The transcript is delivered while the user is still speaking (interim results for latency) and finalized at turn end. Reconnection is transparent to the rest of the pipeline.

---

## Background

All design questions resolved in EXP-006:

- **EXP-006**: Google STT gRPC streaming with 500ms replay buffer for transparent reconnect. The gRPC stream is session-scoped; it starts on turn start and closes on turn end. A 500ms ring buffer of recent PCM frames is replayed to the new stream on reconnect so no audio is lost.

**Existing reference**: `experiments/gato/experiments/exp-008/stt.go` and `experiments/gato/experiments/exp-006/` for the replay buffer design.

---

## Tasks

### Task 4.1 — STTClient interface

Create `gato/internal/stt/stt.go` with a provider-agnostic interface:

```go
type Transcript struct {
    Text     string
    IsFinal  bool
    Stability float32  // 0–1; Google-specific, 0 for other providers
}

type STTClient interface {
    // Start begins streaming. Must be called once before SendAudio.
    Start(ctx context.Context) error
    
    // SendAudio delivers 16kHz mono s16le PCM bytes.
    SendAudio(pcm []byte) error
    
    // Results returns a channel that receives transcripts.
    // Closed when the stream ends.
    Results() <-chan Transcript
    
    // Close signals end of audio and waits for the final transcript.
    Close()
}
```

This interface lets the Agent SDK test with a stub STT client and lets operators swap providers without touching the pipeline.

### Task 4.2 — Google STT implementation

Create `gato/internal/stt/google.go`.

This is a port of `experiments/gato/experiments/exp-008/stt.go`. Key implementation details:

**Client creation**: `speech.NewClient(ctx)` uses Application Default Credentials. The streaming config is:

```go
streamingConfig := &speechpb.StreamingRecognitionConfig{
    Config: &speechpb.RecognitionConfig{
        Encoding:        speechpb.RecognitionConfig_LINEAR16,
        SampleRateHertz: 16000,
        LanguageCode:    "en-US",
        EnableAutomaticPunctuation: true,
    },
    InterimResults: true,
}
```

**Two goroutines per `Start()` call**:
1. `sendLoop`: reads from an internal `chan []byte`, sends `StreamingRecognizeRequest{AudioContent: chunk}` to gRPC
2. `recvLoop`: reads `StreamingRecognizeResponse`, extracts transcripts, sends to `Results()` channel

**Reconnect on gRPC stream timeout**: Google STT streams expire after ~5 minutes. The implementation must detect the `EOF` or `Canceled` error, replay the 500ms ring buffer to the new stream, and continue without the caller knowing. This is transparent to the turn state machine.

**500ms replay buffer**: a ring buffer of the last 500ms of PCM chunks (500ms at 16kHz = 8000 samples = 16KB). On reconnect, replay the buffer as the first audio sent to the new stream. See EXP-006 for the exact ring buffer design.

### Task 4.3 — Session integration

In `Session.handleInputTrack`, after a `TurnStartEvent`:

1. Create a new `STTClient` (Google impl)
2. Call `stt.Start(ctx)`
3. Replay the VAD stream's 500ms audio history to the new STT stream (share the ring buffer between VAD accumulation and STT replay)
4. While `inTurn`, for each VAD chunk: `stt.SendAudio(pcmBytes)`
5. Start a goroutine that reads from `stt.Results()` and for each final transcript: call `session.handleSTTResult(transcript.Text)`

On `TurnEndEvent`:
1. `stt.Close()` — waits for the final transcript to arrive

The STT client is per-turn (created on turn start, destroyed on turn end). Do not reuse it across turns.

### Task 4.4 — Language and model config

Add to the config package:

| Env Var | Default | Description |
|---------|---------|-------------|
| `GATO_STT_LANGUAGE` | `en-US` | BCP-47 language code |
| `GATO_STT_MODEL` | `latest_long` | Google model variant |
| `GATO_STT_INTERIM` | `true` | Enable interim results |

For multi-language deployments, the language code can be set per-deployment via the session `init_payload` (from Switchboard's `SessionReady` message). The Agent SDK (Epic 05) surfaces `init_payload` to customer code; customer code can call `session.SetSTTLanguage(lang)` before the first turn starts.

### Task 4.5 — Error handling and metrics

On gRPC error that is NOT a reconnectable error (e.g. `PERMISSION_DENIED`, `INVALID_ARGUMENT`):
- Log at error level with `session_id` and error code
- Emit `gato_stt_errors_total` counter with label `code=<grpc_status>`
- Send a `STTErrorFrame` upstream (Epic 07) — the Agent SDK can choose to continue or end the session

Track:
- `gato_stt_stream_reconnects_total` — how often the 5-minute timeout triggers
- `gato_stt_transcript_latency_seconds` — time from `TurnEndEvent` to final transcript received (histogram)
- `gato_stt_interim_results_total` — count of interim transcripts delivered

---

## Definition of Done

- [ ] Google STT delivers final transcripts for all turns in the EXP-010 reference 74-second WAV
- [ ] Transcripts match expected text within reasonable accuracy (WER < 10% on the reference recording)
- [ ] gRPC stream reconnects transparently (no lost words after reconnect)
- [ ] STT error (wrong credentials) is logged and metered; session continues to accept new turns
- [ ] Memory stable: no goroutine leak after repeated turn start/end cycles

---

## Verification

### Unit Tests

- `TestGoogleSTT_FinalTranscript`: use `httptest`/gRPC interceptor to inject a fake response; assert `Results()` delivers the transcript and `IsFinal=true`
- `TestGoogleSTT_InterimThenFinal`: inject two responses (`IsFinal=false` then `IsFinal=true`); assert channel delivers both
- `TestGoogleSTT_ReplayBuffer`: create a ring buffer; feed 600ms of audio; trigger reconnect; assert the first 500ms is replayed to the new stream
- `TestSTTClient_Close_WaitsForFinal`: call `Close()` before the final transcript arrives; assert `Close()` blocks until the `recvLoop` drains
- `TestSTTInterface_StubImpl`: implement `STTClient` with a stub; verify the session pipeline works without Google credentials

### Integration Tests

- `TestGoogleSTT_RealStream`: load the `experiments/gato/experiments/exp-010/output/reference_output.wav`, convert to 16kHz mono, feed to `GoogleSTTClient`; assert transcript contains expected words (requires `GOOGLE_APPLICATION_CREDENTIALS` in CI)
- `TestSTT_GoroutineLeak`: start/close 20 STTClients back to back; assert `runtime.NumGoroutine()` returns to baseline after all are closed

### E2E

Full pipeline E2E (aiortc client + Gato server): observe in Gato logs:
- `[stt] "hello world"` or similar transcript on each turn
- No `[stt] SendAudio: ...` errors
- `gato_stt_transcript_latency_seconds` p99 < 2s (from turn end to final transcript)
