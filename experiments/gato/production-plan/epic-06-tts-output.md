# Epic 06: Speech Synthesis & Audio Output

## Business Meaning

After the LLM produces a response, Gato must speak it back to the user in real time. This means synthesizing speech (TTS), resampling it to 48kHz, chunking it into precise 10ms Opus frames, pacing them with a monotonic clock, and handling interruptions mid-utterance — stopping within one chunk (10ms) when the user starts speaking again.

---

## Background

All design decisions proven in experiments:

- **EXP-002**: `LinearResampler` — pure Go, 24→48kHz, 61.5dB SNR. No CGO needed for resampling. Reference: `experiments/gato/experiments/exp-002/resampler.go`.
- **EXP-003**: `AudioQueue` — mutex+cond+wakeOnCancel goroutine. `EndFrame` is uninterruptible (atomic counter). Reference: `experiments/gato/experiments/exp-003/queue.go`.
- **EXP-007**: Monotonic clock targeting eliminates 10% drift from `time.Sleep`. `StopAudioFrame` resets the target clock.
- **EXP-010 (critical bug)**: Last TTS chunk must be zero-padded to exactly 960 bytes. Opus encoder rejects any other frame size with `OPUS_BAD_ARG`.
- **EXP-008**: Full output pipeline reference: `experiments/gato/experiments/exp-008/pipeline.go:298-413`.

---

## Tasks

### Task 6.1 — TTSClient interface and Google implementation

Create `gato/internal/tts/tts.go`:

```go
type TTSClient interface {
    // Synthesize returns 24kHz mono s16le PCM bytes.
    Synthesize(ctx context.Context, text string) ([]byte, error)
}
```

Create `gato/internal/tts/google.go`. This is a direct port of `experiments/gato/experiments/exp-008/tts.go`.

Config:
| Env Var | Default | Description |
|---------|---------|-------------|
| `GATO_TTS_VOICE` | `en-US-Journey-F` | Google TTS voice name |
| `GATO_TTS_LANGUAGE` | `en-US` | BCP-47 language code |
| `GATO_TTS_SPEAKING_RATE` | `1.0` | Speaking rate (0.25–4.0) |

The `Synthesize` method:
1. Calls `texttospeech.NewClient(ctx)` with Application Default Credentials
2. Requests `LINEAR16` encoding at 24kHz
3. Returns the `AudioContent []byte` from the response

For production: add a per-session client that reuses the gRPC connection (don't create a new client on every call). Cache the client in `Session` and close it in `Session.Close()`.

### Task 6.2 — LinearResampler

Port `experiments/gato/experiments/exp-002/resampler.go` to `gato/internal/audio/resampler.go` with no changes to the algorithm. The resampler uses linear interpolation at a 2:1 integer ratio (24→48kHz). It has an internal state for the last sample of the previous call so it can interpolate across chunk boundaries.

```go
type LinearResampler struct {
    lastSample int16
    hasLast    bool
}

func (r *LinearResampler) Resample(in []byte) []byte  // 24kHz s16le → 48kHz s16le
func (r *LinearResampler) Reset()                      // call before a new TTS segment
```

This is already proven at 61.5dB SNR. Do not replace it with a different algorithm without running the SNR benchmark.

### Task 6.3 — AudioQueue

Port `experiments/gato/experiments/exp-003/queue.go` to `gato/internal/audio/queue.go`.

```go
type Frame interface{ isFrame() }

type AudioFrame struct {
    Data        []byte  // exactly 960 bytes (480 int16 samples at 48kHz)
    SegmentText string  // non-empty only on the last chunk of a TTS segment
    IsLastChunk bool
}

type StopAudioFrame struct{}  // bot utterance complete; reset clock target
type EndAudioFrame struct{}   // session shutting down; exit audioTaskRun

type AudioQueue struct {
    mu       sync.Mutex
    cond     *sync.Cond
    frames   []Frame
    closed   bool
    endCount int32  // atomic; EndAudioFrame is uninterruptible
}

func NewAudioQueue() *AudioQueue
func (q *AudioQueue) Put(f Frame)
func (q *AudioQueue) Get(ctx context.Context) (Frame, error)  // blocks; returns nil on close
func (q *AudioQueue) Reset()   // drains all frames EXCEPT EndAudioFrames
func (q *AudioQueue) Close()   // puts an EndAudioFrame; unblocks all Gets
```

`Reset()` is called on interrupt. It must drain all `AudioFrame` and `StopAudioFrame` entries but leave any pending `EndAudioFrame` in the queue (uninterruptible). This is the semantic from EXP-003 using an atomic counter for pending `EndAudioFrame` entries.

`wakeOnCancel(ctx, q)` is a goroutine started per-session that calls `q.Close()` when the context is cancelled, so `Get()` unblocks promptly on shutdown.

### Task 6.4 — TTS segment enqueuing

In `Session.handleSTTResult(transcript string)` (or equivalently, in `Session.Speak(text)`):

1. Reset interrupt flag: `s.interrupted.Store(0)`
2. `s.resampler.Reset()`
3. `pcm24, err := s.tts.Synthesize(ctx, text)` — may take 500ms–2s
4. `pcm48 := s.resampler.Resample(pcm24)` — instant
5. Chunk into 960-byte pieces:

```go
for i := 0; i < len(pcm48); i += 960 {
    end := i + 960
    if end > len(pcm48) {
        end = len(pcm48)
    }
    isLast := end >= len(pcm48)

    chunk := make([]byte, 960)  // always 960, zero-padded
    copy(chunk, pcm48[i:end])   // EXP-010 fix: pad last chunk with silence

    s.audioQueue.Put(&AudioFrame{
        Data:        chunk,
        SegmentText: func() string { if isLast { return text } return "" }(),
        IsLastChunk: isLast,
    })
}
s.audioQueue.Put(&StopAudioFrame{})
```

6. The `audioTaskRun` goroutine dequeues and plays the frames in real time

### Task 6.5 — Streaming TTS (LLM output)

For production LLM integrations that stream tokens, waiting for the full response before synthesizing introduces unacceptable latency. Implement `Session.SpeakStream`:

1. Sentence buffer: accumulate tokens until a sentence boundary (`.`, `?`, `!`, or `\n`)
2. On each sentence: call `tts.Synthesize(ctx, sentence)` and enqueue audio
3. Start playing audio before the LLM finishes generating

This reduces time-to-first-audio from ~3s (wait for full response) to ~500ms (first sentence ready).

Implementation:
```go
func (s *Session) SpeakStream(ctx context.Context, tokens <-chan string) (heardText string, err error) {
    var buf strings.Builder
    for token := range tokens {
        buf.WriteString(token)
        if isSentenceBoundary(token) {
            sentence := buf.String()
            buf.Reset()
            // synthesize and enqueue in background goroutine
        }
    }
    // flush remaining
    if buf.Len() > 0 {
        // synthesize and enqueue last sentence
    }
    // wait for audioQueue to drain
}
```

Track when the last frame plays using the `IsLastChunk` + `SegmentText` marker.

### Task 6.6 — Metrics

Track:
- `gato_tts_latency_seconds` histogram — time from `Speak()` call to first audio frame enqueued
- `gato_tts_errors_total` counter — failed `Synthesize` calls
- `gato_audio_queue_depth` gauge — current frames in queue (sampled at audioTaskRun tick)
- `gato_interrupts_total` counter — how often playback is interrupted mid-utterance

---

## Definition of Done

- [ ] Google TTS synthesizes speech; browser plays it at intelligible quality
- [ ] 24→48kHz resampling produces no audible artifacts (SNR ≥ 60dB, verified with the EXP-002 test)
- [ ] Last chunk is always 960 bytes: no `opus: invalid argument` errors in logs
- [ ] Interrupt stops playback within 10ms (one chunk): `audioQueue.Reset()` called within 10ms of `TurnStartEvent`
- [ ] [HEARD] text is accurate on interruption (see Epic 03)
- [ ] `SpeakStream` plays first audio before LLM finishes (first sentence enqueued ≤ 800ms after first token)

---

## Verification

### Unit Tests

- `TestLinearResampler_SNR`: generate 1kHz sine at 24kHz; resample to 48kHz; compute SNR; assert ≥ 60dB
- `TestLinearResampler_CrossChunkInterpolation`: split a sine wave into two chunks; resample each; assert no discontinuity at the boundary (last sample of chunk 1 interpolated with first of chunk 2)
- `TestAudioQueue_Reset_PreservesEndFrame`: put 5 `AudioFrame` + 1 `EndAudioFrame`; call `Reset()`; assert next `Get()` returns `EndAudioFrame` (not blocked)
- `TestAudioQueue_WakeOnCancel`: block on `Get()` in goroutine; cancel context; assert `Get` returns within 1ms
- `TestEnqueue_LastChunkPadded`: synthesize 1000 bytes of PCM (not a multiple of 960); assert all enqueued chunks are exactly 960 bytes
- `TestEnqueue_SegmentText`: assert `SegmentText` is set only on the last chunk

### Integration Tests

- `TestTTS_GoogleRoundTrip`: call `Synthesize("Hello")` with real Google credentials; assert returned PCM is non-empty; play back with a simple decoder and verify no Opus encode errors
- `TestSpeakStream_FirstAudioLatency`: stub TTS with 200ms delay; stream 10 tokens with sentence boundary at token 5; assert audio queue has frames within 300ms (not waiting for all 10 tokens)

### E2E

Run full pipeline (aiortc client → Gato → TTS → client):
- Browser hears intelligible speech (manual verification with `afplay`)
- `reference_output.wav` from EXP-010 regenerated and compared: duration similar, volume within 3dB
- Server logs show zero `opus encode` errors, zero `tts: Synthesize` errors
