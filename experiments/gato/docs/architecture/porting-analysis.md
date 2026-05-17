# Gato — Pipecat Porting Analysis

Derived from tracing the transitive imports of a minimal voice bot (see `../../bot.go`).
Python source line counts are from the pipecat main branch as of this writing.

---

## 1. Pipecat Code Surface Imported by a Minimal Voice Bot

The Python bot.py (8 processors, 1 pipeline, 1 task) transitively touches:

| File (Python)                                    | Lines | Role                                         |
|--------------------------------------------------|-------|----------------------------------------------|
| `processors/frame_processor.py`                  | 1074  | Base class every processor inherits           |
| `pipeline/task.py`                               | 1044  | PipelineTask: orchestration, heartbeat, idle  |
| `transports/base_output.py`                      | 995   | BaseOutputTransport + MediaSender             |
| `frames/frames.py`                               | 1997  | 236 frame type definitions                   |
| `pipeline/pipeline.py`                           | ~300  | Pipeline: prev/next wiring, setup/teardown    |
| `transports/smallwebrtc/transport.py`            | ~450  | RawAudioTrack, SmallWebRTCClient              |
| `transports/base_input.py`                       | 265   | BaseInputTransport: audio queue + filter      |
| `utils/frame_queue.py`                           | 94    | Interrupt-aware queue (UninterruptibleFrame)  |
| `utils/asyncio/task_manager.py`                  | ~200  | create_task / cancel_task with tracking       |
| `utils/base_object.py`                           | ~200  | Event system (register_event_handler)         |
| `clocks/system_clock.py`                         | ~50   | Nanosecond clock for PTS-based frame delivery |
| `audio/utils.py`                                 | ~150  | create_stream_resampler, is_silence           |
| **Total (framework)**                            | **~5800** |                                           |

---

## 2. Framework vs Implementation Split

### Framework (must port — generic infrastructure)

These are the pieces that have no provider-specific logic. Every processor,
every transport, every service depends on them.

| Gato Go package                        | Ported from (Python)                  | Notes                                              |
|----------------------------------------|---------------------------------------|----------------------------------------------------|
| `gato/frames`                          | `frames/frames.py`                    | ~25 types needed (not 236); skip LLM, video, telephony |
| `gato/pipeline`                        | `processors/frame_processor.py`       | Priority queue → two Go channels per processor     |
| `gato/pipeline/queue`                  | `utils/frame_queue.py`                | Mutex-backed slice, not a plain channel            |
| `gato/pipeline/task`                   | `pipeline/task.py`, `pipeline/pipeline.py` | PipelineSource + PipelineSink + heartbeat goroutine |
| `gato/transport`                       | `transports/base_input.py` + `base_output.py` | The bulk of the hard work                   |
| `gato/observers`                       | `observers/base_observer.py`          | on_push_frame / on_process_frame callbacks         |

Estimated Go LOC: **3000–3500 lines** (less dense than Python but more explicit types).

### Provider implementations (build new, do not port)

These replace pipecat's provider-specific services but follow the same frame contract.

| Gato Go package                        | Replaces (Python)                     | Notes                                              |
|----------------------------------------|---------------------------------------|----------------------------------------------------|
| `gato/transport/pion`                  | `transports/smallwebrtc/transport.py` | Pion replaces aiortc; clock model differs          |
| `gato/audio/vad`                       | `audio/vad/silero.py`                 | CGO/ONNX; same frame contract                      |
| `gato/audio/turn`                      | livekit-agents `endpointing.py`       | CGO/ONNX; new in Gato                              |
| `gato/services/stt/google`             | `services/deepgram/stt.py`            | gRPC streaming; same TranscriptionFrame contract   |
| `gato/services/tts/google`             | `services/cartesia/tts.py`            | HTTP/2 streaming; same TTSAudioRawFrame contract   |
| `gato/processors/agg`                  | `processors/aggregators/llm_response_universal.py` | Simpler: no LLM context, no tool calls |
| `gato/processors/ipc`                  | *(no equivalent)*                     | New: bidirectional protobuf IPC bridge             |

---

## 3. Risk Assessment

### 🔴 HIGH — BaseOutputTransport.MediaSender (audio output pacing)

**What it is:** The `MediaSender` inner class in `base_output.py` is 600+ lines of carefully
tuned audio machinery:

1. **Chunk normalization**: incoming TTS audio may be large blocks (1–2 seconds). Must be split
   into `audio_chunk_size` pieces (default: 10 ms) before queuing. This bounds interrupt latency:
   if audio is one large chunk, you cannot interrupt mid-chunk.

2. **Streaming resampler**: TTS outputs at 24 kHz; Pion's Opus codec internally operates at 48 kHz.
   Must resample every chunk. Python uses `libsamplerate` (streaming, stateful). In Go there is
   no standard streaming resampler. Options: CGO-wrap libsamplerate (adds CGO dependency), or
   write a linear interpolation resampler (acceptable quality for voice).
   **Mitigation**: configure Google TTS to output at 24 kHz and configure Pion codec at 24 kHz
   if the codec permits it, eliminating the need for resampling entirely.

3. **Real-time audio pacing**: in pipecat + aiortc, WebRTC's `recv()` pull model drives the clock
   (the audio track waits for WebRTC to ask for the next frame). In Pion, it is push: we call
   `track.WriteSample()`. If we push all chunks at once, the client receives them ahead of time and
   may jitter-buffer them correctly, but interrupt latency becomes undefined. Correct approach:
   sleep 10 ms between each 10 ms chunk in the audio task goroutine. This keeps the sender
   approximately one chunk ahead of playback, bounding interrupt latency to 10 ms.

4. **BotStartedSpeaking / BotStoppedSpeaking state machine**: driven by `TTSStoppedFrame`
   (explicit signal) or silence detection on `SpeechOutputAudioRawFrame`. Each event pushes
   frames both downstream AND upstream simultaneously. Upstream `BotStoppedSpeakingFrame` is
   what the assistant aggregator uses to finalize the assistant turn. Getting the direction wrong
   breaks turn tracking.

5. **FrameQueue interruption decision**: on `InterruptionFrame` arrival:
   - If queue `has_uninterruptible` (e.g. `EndFrame` is queued) → call `Reset()` to drain only
     interruptible frames; keep the audio task alive to drain the uninterruptible ones.
   - Otherwise → cancel the audio task goroutine and recreate it with a fresh queue.
   Getting this wrong either: loses `EndFrame` (session never terminates cleanly), or bot does
   not stop speaking on interrupt.

**Estimated Go implementation**: 700–900 lines, 3–5 days of careful work.

---

### 🔴 HIGH — FrameQueue (interrupt-safe queue)

**What it is:** A queue that tracks whether any `UninterruptibleFrame` is in-flight, and exposes
`Reset()` to atomically drain interruptible items while preserving uninterruptible ones.

**The Go problem**: plain Go channels cannot be inspected or partially drained. A buffered
`chan Frame` cannot support `has_uninterruptible` in O(1) or `Reset()` without scanning.

**Required**: a mutex-protected slice with `put()`, `get()` (blocking), `hasUninterruptible()`,
and `reset()`. This is not complex to write (~100 lines) but it is a departure from idiomatic
Go channels and must be used correctly everywhere in the output transport.

---

### 🔴 HIGH — Streaming audio resampler

**What it is**: pipecat's `create_stream_resampler()` returns a stateful resampler that can
convert arbitrary-length audio chunks from one sample rate to another incrementally. This is
necessary because TTS chunks arrive in irregular sizes.

**Go options:**
- CGO-wrap `libsamplerate` — correct, but adds another CGO dependency alongside ONNX
- `github.com/dh1tw/streamdecode` — limited
- Custom linear interpolation — acceptable for voice, ~150 lines Go

**Mitigation:** Match TTS output rate to transport rate at configuration time to eliminate
resampling entirely. Confirm what Pion accepts for Opus before writing a resampler.

---

### 🟠 MEDIUM — FrameProcessor priority queue

**What it is**: each processor has a `FrameProcessorQueue` (priority queue) that gives
`SystemFrame` objects HIGH priority over `DataFrame` objects, preserving FIFO within each tier.

**Go translation**: two buffered channels per processor (`systemChan`, `dataChan`) plus an
input goroutine that drains system frames first via a nested `select`:

```go
// input goroutine: priority dispatch to processChan
for {
    select {
    case f := <-systemChan:   // system frames always first
        processChan <- f
    default:
        select {
        case f := <-systemChan:
            processChan <- f
        case f := <-dataChan:
            processChan <- f
        }
    }
}
```

This is idiomatic Go and actually cleaner than Python's PriorityQueue. Medium risk because
the nested `select` behaviour must be tested — it doesn't guarantee strict starvation-freedom
of data frames, but that is acceptable (system frames are rare).

---

### 🟠 MEDIUM — PipelineTask (source / sink / heartbeat)

**What it is**: `PipelineTask` wraps the user pipeline with a `PipelineSource` (entry point for
`task.queue_frames()`) and a `PipelineSink` (heartbeat, error collection, idle detection).
StartFrame is injected by the task, not the user. Observers are registered on the task and
receive callbacks on every frame push across all processors.

Observer fan-out at 20 ms audio granularity: at 50 audio frames/second per session with
N sessions, observers add O(N × 50) callbacks/second. Must be non-blocking in the call path;
observer goroutines should receive frames via their own channel.

**Estimated Go implementation**: 400–500 lines. Well-understood structure.

---

### 🟠 MEDIUM — Google Cloud STT (gRPC streaming)

**What it is**: bidirectional gRPC stream. Gato sends audio chunks; Google sends interim and
final `StreamingRecognitionResult` messages. Stream must be restarted when:
- Google closes it (5-minute hard limit)
- VAD detects a long silence
- An error occurs

**Known issues in production**: the stream restart creates a brief window where audio is
not being transcribed. Must buffer a few frames around the restart. Google's gRPC Go client
is mature. Estimated 2–3 days including restart handling.

---

### 🟠 MEDIUM — [HEARD] exact text tracking in AssistantAggregator

**What it is**: on `InterruptionFrame`, the business layer receives `TurnInterrupted(heard_text)`
where `heard_text` is the exact TTS text that completed playout before the interrupt fired.

This requires: as each `TTSTextFrame` is queued to the output transport, the assistant aggregator
tracks which text frames have been fully played (i.e., their corresponding `TTSAudioRawFrame`
reached the output and the audio task confirmed it). The text that completed is derived from the
audio task, not estimated.

In pipecat-adk this is done via turn_id correlation. In Gato: similar mechanism — each
`TTSAudioRawFrame` carries the text it was derived from, and the audio task pushes a
`TTSChunkCompletedFrame` upstream when the chunk is sent. The assistant aggregator accumulates
completed text and sends it on interruption.

Subtle: interruption arrives before some audio chunks are sent. The audio task must check for
`InterruptionFrame` between every 10 ms chunk in the sleep loop.

---

### 🟢 LOW — Google Cloud TTS (HTTP/2 streaming synthesis)

Standard gRPC streaming in Go. Receives audio chunks as synthesis proceeds. Push each chunk as
`TTSAudioRawFrame` downstream; send `TTSStoppedFrame` when the stream closes. 1–2 days.

---

### 🟢 LOW — UserAggregator / AssistantAggregator

Much simpler than pipecat's `LLMContextAggregatorPair`:
- **UserAggregator**: accumulate interim transcription, on final → emit TurnStarted + TranscriptFinal
  to IPC bridge. On InterruptionFrame → emit TurnInterrupted(heard_text) to IPC bridge.
- **AssistantAggregator**: receive TextChunkFrame from IPC, accumulate, send to TTS. Track which
  text chunks have completed playout for [HEARD] calculation.

No LLM context management, no tool call orchestration. 1–2 days each.

---

### 🟢 LOW — ProtobufIPCBridge

Bidirectional WebSocket. Standard Go. Downstream frames → protobuf → wire; wire → protobuf →
upstream frames. The proto schema follows pipecat's `Frame { oneof payload }` canvas.
Reconnect on disconnect with exponential backoff. 1–2 days.

---

### 🟢 LOW — VAD (SileroVAD, CGO/ONNX)

Pattern established from `~/apps/vad` experiments. CGO wrapper + goroutine pool sized to
`runtime.NumCPU()`. Processes every 20 ms InputAudioRawFrame. Result pushes
`VADSpeechStartFrame` / `VADSpeechEndFrame`. 2–3 days for CGO wiring + goroutine pool.

---

### 🟢 LOW — TurnDetectorV3 (CGO/ONNX)

Same CGO pattern as VAD. Runs only on `VADSpeechEndFrame` (not every 20 ms chunk). Answers
"is this turn complete?" — binary classifier. 1–2 days.

---

## 4. Summary Table

| Component                              | Risk   | Est. Days | Pipecat LOC reference |
|----------------------------------------|--------|-----------|-----------------------|
| BaseOutputTransport + MediaSender      | 🔴 HIGH | 4–5       | 995 lines             |
| FrameQueue (interrupt-safe)            | 🔴 HIGH | 1         | 94 lines              |
| Streaming audio resampler              | 🔴 HIGH | 1–2       | audio/utils.py        |
| FrameProcessor base class              | 🟠 MED  | 3–4       | 1074 lines            |
| PipelineTask + source/sink             | 🟠 MED  | 3–4       | 1044 lines            |
| Google STT (gRPC streaming)            | 🟠 MED  | 2–3       | —                     |
| [HEARD] exact text tracking            | 🟠 MED  | 2         | pipecat-adk           |
| PionWebRTCTransport                    | 🟠 MED  | 3–4       | smallwebrtc/           |
| Google TTS (HTTP/2 streaming)          | 🟢 LOW  | 1–2       | —                     |
| UserAggregator + AssistantAggregator   | 🟢 LOW  | 2–3       | —                     |
| ProtobufIPCBridge                      | 🟢 LOW  | 1–2       | —                     |
| SileroVAD (CGO/ONNX)                  | 🟢 LOW  | 2–3       | —                     |
| TurnDetectorV3 (CGO/ONNX)             | 🟢 LOW  | 1–2       | —                     |
| Frame type definitions (~25 types)     | 🟢 LOW  | 1         | 1997 lines (236 types)|
| **Total**                              |        | **27–39 days** |                   |

---

## 5. Key Mitigations

1. **Eliminate the resampler** by configuring Google TTS to output at the same sample rate
   Pion expects for the Opus codec. Verify whether Pion's Opus encoder accepts 24 kHz input.
   If not, prototype the resampler first — it blocks everything downstream.

2. **Prototype the output transport audio pacing first**, before building other processors.
   A standalone test: Pion PeerConnection + Go goroutine sleeping 10 ms per chunk → browser
   hears smooth audio. Validate interrupt latency by measuring time from "interrupt signal sent"
   to "audio stops playing" in a test harness.

3. **FrameProcessor can start simple**: for hello world, skip metrics, skip observers, skip
   pause/resume. Start with the minimum: input queue goroutine (system priority) + process
   goroutine, push_frame, queue_frame. Add features incrementally.

4. **Bot.go as the integration test**: `bot.go` should be the first end-to-end test.
   Build stubs for each processor that pass frames through, then replace stubs one at a time.
