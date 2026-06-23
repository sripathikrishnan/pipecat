# Epic 07: Pipecat Frame Pipeline

## Business Meaning

Voqalcloud's infrastructure is built on Pipecat's frame-based pipeline model. For Gato to be a first-class citizen — and for customer code to feel familiar to Python Pipecat developers — Gato must implement the same frame semantics in Go: frames flow through a chain of processors, SystemFrames have guaranteed priority, and the pipeline lifecycle (StartFrame, EndFrame, StopFrame) is identical.

This also future-proofs the Agent SDK: customers who use Pipecat's Python framework to prototype can port to Go using the same mental model.

---

## Background

Pipecat's architecture (from `src/pipecat/processors/frame_processor.py` and `src/pipecat/pipeline/pipeline.py`):

- **Frames**: typed data units flowing through the pipeline (AudioRawFrame, TextFrame, LLMMessagesFrame, etc.)
- **FrameProcessor**: receives frames from upstream, processes, pushes downstream. Some processors push frames upstream (acknowledgements, errors).
- **Pipeline**: chains processors; passes frames along.
- **Direction**: `DOWNSTREAM` (input → output) or `UPSTREAM` (acknowledgements, errors)
- **SystemFrames**: have priority over data frames. Examples: `StartFrame`, `StopFrame`, `EndFrame`, `CancelFrame`.

From EXP-005: Go's `select` does NOT guarantee priority. A mutex-backed priority queue with separate system/data slices is required to guarantee SystemFrame delivery before DataFrames.

**Existing references**:
- Python frame types: `src/pipecat/frames/frames.py`
- Python FrameProcessor: `src/pipecat/processors/frame_processor.py`
- Python Pipeline: `src/pipecat/pipeline/pipeline.py`
- EXP-005 priority queue design: `experiments/gato/experiments/exp-005/`

---

## Tasks

### Task 7.1 — Core frame types

Create `gato/internal/pipeline/frames.go`.

The Go frame hierarchy mirrors Pipecat's Python frames. Every frame implements the `Frame` interface:

```go
type Frame interface {
    frameName() string   // for logging/debugging
    isSystemFrame() bool // SystemFrames get priority queue treatment
}

// ---- System Frames ----
type StartFrame   struct{}  // pipeline is starting; sent first by PipelineTask
type StopFrame    struct{}  // soft stop; processors should flush then stop
type EndFrame     struct{}  // hard stop; processors should stop immediately
type CancelFrame  struct{}  // cancel in-flight operations

// ---- Data Frames ----
type AudioRawFrame struct {
    Audio      []byte  // s16le PCM
    SampleRate int
    Channels   int
}

type TranscriptionFrame struct {
    Text      string
    UserID    string
    Timestamp time.Time
}

type TTSTextFrame    struct{ Text string }
type TTSAudioFrame   struct{ Audio []byte; SampleRate int }
type LLMTextFrame    struct{ Text string }

type ErrorFrame struct {
    Error error
    Fatal bool
}

type InterruptionFrame struct {
    HeardText string
    done      chan struct{}  // closed when frame reaches the sink
}

func (f *InterruptionFrame) Complete() { close(f.done) }
func (f *InterruptionFrame) Wait() <-chan struct{} { return f.done }
```

Pipecat has 100+ frame types. For the initial implementation, implement only the types needed by the core path (audio in, STT, LLM, TTS, audio out). Add more as needed.

### Task 7.2 — FrameProcessor interface

Create `gato/internal/pipeline/processor.go`.

```go
type Direction int
const (
    Downstream Direction = iota
    Upstream
)

type FrameProcessor interface {
    // Name returns a human-readable name for logging.
    Name() string

    // ProcessFrame is called for each frame. The processor should:
    //   1. Inspect the frame
    //   2. Optionally transform it
    //   3. Push result(s) downstream or upstream via PushFrame
    // Do not hold the frame; return promptly.
    ProcessFrame(ctx context.Context, frame Frame, dir Direction) error

    // SetDownstream/SetUpstream connect adjacent processors.
    SetDownstream(p FrameProcessor)
    SetUpstream(p FrameProcessor)
}

// BaseProcessor implements the wiring; embed it in concrete processors.
type BaseProcessor struct {
    name       string
    downstream FrameProcessor
    upstream   FrameProcessor
    queue      *PriorityQueue  // from EXP-005
}

func (b *BaseProcessor) PushDownstream(ctx context.Context, f Frame) error
func (b *BaseProcessor) PushUpstream(ctx context.Context, f Frame) error
```

### Task 7.3 — Priority queue (from EXP-005)

Create `gato/internal/pipeline/priority_queue.go`. Port the EXP-005 design exactly.

The key finding: Go's `select` over two channels does NOT guarantee the system channel drains first. The correct design is a single mutex-protected queue with two slices:

```go
type PriorityQueue struct {
    mu      sync.Mutex
    cond    *sync.Cond
    system  []queueItem   // SystemFrame entries; always dequeued first
    data    []queueItem   // DataFrame entries; dequeued only when system is empty
    closed  bool
}

type queueItem struct {
    frame Frame
    dir   Direction
}

func (q *PriorityQueue) Push(f Frame, dir Direction)
func (q *PriorityQueue) Pop(ctx context.Context) (queueItem, error)
func (q *PriorityQueue) Close()
```

`Pop` blocks when both slices are empty; returns `system[0]` if non-empty, else `data[0]`.

This is the correctness guarantee: an `EndFrame` (SystemFrame) enqueued after 1000 `AudioFrame` (DataFrames) will be dequeued before any of the AudioFrames.

### Task 7.4 — Pipeline

Create `gato/internal/pipeline/pipeline.go`.

```go
type Pipeline struct {
    processors []FrameProcessor
}

// NewPipeline chains processors: output of [i] → input of [i+1].
func NewPipeline(processors ...FrameProcessor) *Pipeline

// Run starts processing. Blocks until EndFrame propagates to the last processor
// or ctx is cancelled.
func (p *Pipeline) Run(ctx context.Context) error

// Push sends a frame into the first processor (source).
func (p *Pipeline) Push(ctx context.Context, f Frame) error
```

The pipeline sends `StartFrame` at the beginning of `Run()` and `EndFrame` at context cancellation. All processors must handle these lifecycle frames.

### Task 7.5 — Core processors

Implement the minimal set of processors that compose the Gato voice pipeline. Each is a `FrameProcessor`:

**`AudioInputProcessor`**: wraps the WebRTC input track. Produces `AudioRawFrame` (48kHz Opus-decoded PCM) downstream.

**`VADProcessor`**: consumes `AudioRawFrame`, runs VAD, emits `UserStartedSpeakingFrame` and `UserStoppedSpeakingFrame` upstream. Maintains the 64-sample context buffer and turn state machine from Epic 03.

**`STTProcessor`**: consumes `AudioRawFrame` while in turn, produces `TranscriptionFrame` downstream.

**`LLMContextProcessor`**: accumulates `TranscriptionFrame` into an LLM context; invokes customer's LLM function; emits `TTSTextFrame` (or `TTSTextFrame` chunks for streaming).

**`TTSProcessor`**: consumes `TTSTextFrame`, produces `TTSAudioFrame` (24kHz PCM).

**`AudioOutputProcessor`**: consumes `TTSAudioFrame`, resamples to 48kHz, chunks, enqueues to `AudioQueue`. Writes to the WebRTC output track via Opus encoder.

### Task 7.6 — Interruption frame semantics

Match Pipecat's interruption semantics (from `CLAUDE.md`):

When an `InterruptionFrame` is pushed downstream:
- Each processor that stops the frame from propagating (i.e., handles it and does not re-push) MUST call `frame.Complete()`
- If the frame reaches the sink (last processor), the sink calls `frame.Complete()`
- The `PushInterruptionAndWait(ctx, frame)` helper sends the frame and blocks on `frame.Wait()`

This ensures that callers who need to know when the interrupt has fully propagated (e.g., before starting a new TTS utterance) can wait deterministically.

---

## Definition of Done

- [ ] `NewPipeline(processors...).Run(ctx)` drives frames through the chain
- [ ] `EndFrame` and `StopFrame` always reach all processors before pending `AudioRawFrame` entries (priority guarantee from EXP-005)
- [ ] `InterruptionFrame.Complete()` is called by every processor that consumes it without propagating
- [ ] The core voice pipeline (AudioInput → VAD → STT → LLM → TTS → AudioOutput) runs as a Pipeline with no special-casing
- [ ] A customer can insert a custom `FrameProcessor` into the pipeline (e.g., a logging processor) without modifying Gato internals

---

## Verification

### Unit Tests

- `TestPriorityQueue_SystemFirst`: enqueue 100 `AudioRawFrame` then 1 `EndFrame`; assert `Pop()` returns `EndFrame` next (not an `AudioRawFrame`)
- `TestPriorityQueue_BlocksOnEmpty`: call `Pop` with no frames; assert it blocks; push one frame; assert it unblocks within 1ms
- `TestPipeline_LifecycleSEF`: create a recording processor; run pipeline; assert frames received in order: `StartFrame`, data frames, `EndFrame`
- `TestPipeline_InterruptionComplete`: create a processor that handles `InterruptionFrame` without propagating; assert `frame.Complete()` called
- `TestBaseProcessor_PushUpstream`: push a frame upstream; assert it reaches the processor before the source

### Integration Tests

- `TestVoicePipeline_HappyPath`: construct the full 6-processor pipeline with stub STT/TTS; inject an `AudioRawFrame` stream with speech; assert `TranscriptionFrame` emitted, then `TTSTextFrame`, then `TTSAudioFrame`
- `TestVoicePipeline_Interrupt`: inject speech mid-TTS; assert `InterruptionFrame` propagates; assert TTS audio stops; assert `Complete()` called
- `TestPipeline_CustomProcessor`: inject a no-op logging processor between STT and LLM; assert all frames pass through correctly

### E2E

The pipeline is exercised by the full aiortc E2E test. Confirm via Gato structured logs:
- `[pipeline] StartFrame` on session start
- `[pipeline] TranscriptionFrame text="..."` on each turn
- `[pipeline] InterruptionFrame heard="..."` on interruption
- `[pipeline] EndFrame` on session end
