# Epic 03: VAD & Turn Detection

## Business Meaning

Gato must know when the user is speaking and when they've finished. Without this, Gato either misses speech entirely or triggers a response after every ambient noise. VAD (voice activity detection) is the gatekeeper: it decides when a "turn" starts (user is speaking) and when it ends (user has been silent long enough). The [HEARD] mechanism adds a precise accounting of exactly what the bot said before being interrupted — critical for building coherent conversations.

---

## Background

All architectural questions were resolved in the experiments:

- **EXP-004**: Strategy B — one shared ONNX session across all concurrent sessions, per-stream `StreamState [2,1,128] float32`. Inference cost 0.08ms p99 single-stream, 4.41ms p99 at N=50.
- **EXP-007**: [HEARD] = last-chunk-dequeue marker. Monotonic clock (not `time.Sleep`) prevents 10% drift per chunk on macOS arm64.
- **EXP-010 (critical bug)**: Silero VAD v5 requires 576 samples per `Infer()` call, not 512. The outer model prepends 64 context samples from the previous call before feeding the inner LSTM. Without this context, all probabilities are ~0.0005 regardless of audio content.

**Existing reference**: `experiments/gato/experiments/exp-008/vad.go` and `experiments/gato/experiments/exp-008/pipeline.go:143-293`.

---

## Tasks

### Task 3.1 — SileroVAD struct (shared ONNX session)

Create `gato/internal/vad/silero.go`. This is a direct port of `experiments/gato/experiments/exp-008/vad.go`.

```go
const ContextSize = 64               // samples prepended from previous call
const ChunkSize   = 512              // samples per VAD chunk (32ms at 16kHz)
const InferSize   = ContextSize + ChunkSize  // = 576; what Infer() receives

type StreamState struct {
    H [2][1][128]float32  // LSTM hidden state
    C [2][1][128]float32  // LSTM cell state
}

type SileroVAD struct {
    session *ort.InferenceSession  // shared; read-only after construction
    mu      sync.Mutex             // guards ONNX session from concurrent Infer calls
}

func NewSileroVAD(modelPath string) (*SileroVAD, error)

// Infer requires exactly InferSize=576 samples: first 64 are context from previous call,
// next 512 are the new audio chunk. Returns speech probability in [0, 1].
func (v *SileroVAD) Infer(audio []float32, sampleRate int64, state StreamState) (prob float32, newState StreamState, err error)
```

The `mu` protects the ONNX session because `onnxruntime-go` sessions are not goroutine-safe for concurrent `Run()` calls (see EXP-004 findings). Each `Infer()` call holds the lock for ~0.08ms — acceptable.

The ONNX model file path comes from config: `GATO_VAD_MODEL_PATH` (default: `/opt/gato/silero_vad_v5.onnx`). The model is loaded once at startup and reused for the lifetime of the process.

### Task 3.2 — Per-stream state and context buffer

Each `Session` carries:

```go
type Session struct {
    ...
    vad        *SileroVAD    // shared (pointer to server-level singleton)
    vadState   StreamState    // per-session LSTM state
    vadContext [ContextSize]float32  // last 64 samples of previous chunk
    ...
}
```

After each `Infer()` call:

```go
// Advance context: slide vadContext forward
copy(s.vadContext[:], input[ChunkSize:])
```

This is the fix for the EXP-010 critical bug. Without it, every Infer call starts from zero context and returns near-zero probability for all audio.

### Task 3.3 — VAD chunk accumulation

In `handleInputTrack` (EXP-008 reference, lines 208-228):

1. Decimated 16kHz PCM is appended to `vadBuf []float32`
2. When `len(vadBuf) >= ChunkSize (512)`, extract one chunk
3. Build `input [InferSize]float32`: copy `vadContext` into `[0:64]`, copy chunk into `[64:576]`
4. Call `s.vad.Infer(input[:], vadSampleRate, s.vadState)`
5. Update `s.vadState` and `s.vadContext`
6. Evaluate turn state machine (Task 3.4)

Package this as `gato/internal/audio/vad_stream.go` with a `VADStream` type that owns the accumulation buffer and context, so `handleInputTrack` doesn't carry all this state inline.

### Task 3.4 — Turn state machine

The turn state machine converts a stream of per-chunk probabilities into turn start/end events.

```go
const (
    SpeechThreshold  = 0.5
    SpeechStartCount = 3   // 3 × 32ms = ~96ms speech to start turn
    SpeechEndCount   = 25  // 25 × 32ms = 800ms silence to end turn
)
```

State transitions:

```
NOT_IN_TURN:
  if prob > SpeechThreshold for SpeechStartCount consecutive chunks:
    → IN_TURN; emit TurnStartEvent

IN_TURN:
  if prob <= SpeechThreshold for SpeechEndCount consecutive chunks:
    → NOT_IN_TURN; emit TurnEndEvent
```

The counters reset on direction change (speech → silence resets speech counter; silence → speech resets silence counter).

Emit events via a `chan TurnEvent` on the VADStream so that STT (Epic 04) and interruption handling (Task 3.5) can subscribe independently.

### Task 3.5 — Interruption handling

When a `TurnStartEvent` arrives while the bot is playing audio (`s.interrupted.CompareAndSwap(0, 1)` succeeds):

1. `s.audioQueue.Reset()` — drains the queue, stopping playback within one 10ms chunk
2. `s.resampler.Reset()` — clear any resampler buffered state
3. Read `s.heardText` (protected by `heardMu`) — this is the text of the last completed TTS segment that played before interruption
4. Log `[interrupt] heard: "<text>"` and emit a metric
5. Send `InterruptEvent{HeardText: text}` upstream to the Agent SDK (Epic 05)

The interrupt path must be non-blocking relative to the audio output loop. Use the CAS (`CompareAndSwap`) to ensure only one interrupt fires per turn.

### Task 3.6 — [HEARD] mechanism

The [HEARD] mechanism tracks exactly what the bot said before an interruption. From EXP-007:

In `audioTaskRun`, when dequeuing an `AudioFrame` that has `IsLastChunk=true` and `SegmentText != ""`:

```go
s.heardMu.Lock()
s.heardText = f.SegmentText
s.heardMu.Unlock()
```

`SegmentText` is set by `handleSTTResult` on the final chunk of each TTS response. Only the last chunk of a segment carries text; intermediate chunks carry `""`. This means `heardText` is updated only when a full segment finishes playing — if the user interrupts mid-segment, `heardText` reflects the last *fully played* segment.

The [HEARD] text is surfaced to the Agent SDK as part of the `InterruptEvent`. Customer LLMs can use it for context ("I was saying X when the user interrupted me").

---

## Definition of Done

- [ ] Gato correctly detects speech start/end from a 74-second recording with clear speech (the EXP-010 reference WAV)
- [ ] No false turn starts on silence-only audio (test with 10 seconds of silence)
- [ ] VAD inference latency: p99 < 1ms single session, p99 < 5ms at 10 concurrent sessions (verify with Prometheus histogram)
- [ ] [HEARD] text is accurate: interrupt mid-utterance; assert `heardText` matches the last *completed* segment, not the interrupted one
- [ ] `vadContext` is updated correctly: the 0.0005 probability bug from EXP-010 does not recur

---

## Verification

### Unit Tests

- `TestVADStream_SpeechDetection`: feed 96ms (3 chunks) of speech-level audio (0.8 sine wave at 300Hz); assert `TurnStartEvent` emitted
- `TestVADStream_SilenceAfterSpeech`: emit TurnStart; feed 800ms of silence (25 chunks); assert `TurnEndEvent` emitted
- `TestVADStream_ContextBuffer`: feed one chunk; assert `vadContext` equals last 64 samples of that chunk; feed second chunk prepended with that context; verify `Infer` called with correct 576 samples
- `TestVADStream_NoBugZeroProbability`: load the EXP-010 reference WAV (48kHz → decimate to 16kHz); feed through VAD; assert `maxProb > 0.5` at some point
- `TestInterrupt_HeardText`: set up a session; play two TTS segments; interrupt mid-second; assert `heardText` equals the first segment text
- `TestInterrupt_CompareAndSwap`: send two TurnStart events in rapid succession; assert `handleInterrupt` called exactly once (CAS prevents double-interrupt)

### Integration Tests

- `TestVAD_MultiSession`: create 10 `VADStream` instances sharing one `SileroVAD`; run them concurrently; feed speech audio to each; assert all detect turns; measure inference latency with `time.Since`; assert p99 < 5ms
- `TestVAD_ZeroGoroutineGrowth`: run 100 VAD inference cycles; assert `runtime.NumGoroutine()` is the same before and after

### E2E

Feed the EXP-010 reference 74-second WAV through the full pipeline. Observe in server logs:
- At least 3 `[vad] turn START` events
- At least 3 `[vad] turn END` events
- `[interrupt] heard:` logged if STT+TTS loop fast enough
- No `vad infer: ...` errors in logs
