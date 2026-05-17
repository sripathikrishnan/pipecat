# EXP-003: Interrupt-Safe Audio Queue

**Risk addressed**: FrameQueue semantics — does our Go queue correctly drain interruptible
frames while preserving UninterruptibleFrames (EndFrame) through an interruption?

**Status**: [ ]

**Depends on**: EXP-001, EXP-002 (confirms chunk size and pacing model)

---

## Hypothesis

Go channels cannot be partially drained. The FrameQueue must be a mutex-backed slice that
supports `HasUninterruptible()` in O(1) and `Reset()` (drain interruptible, keep uninterruptible).

The interruption path in the output transport is:
```
InterruptionFrame arrives
  if queue.HasUninterruptible():
      queue.Reset()       // drain audio frames, keep EndFrame
      // audio task keeps running to deliver EndFrame
  else:
      cancel audio goroutine
      restart audio goroutine with fresh queue
```

If `Reset()` is wrong, either: (a) EndFrame is lost → session never terminates, or (b)
audio frames are not drained → bot keeps talking after interrupt.

---

## Program

Pure Go unit test + one integration test. No WebRTC, no Pion.

```
experiments/gato/experiments/exp-003/
  queue.go        — FrameQueue implementation (the artifact under test)
  queue_test.go   — unit tests
  integration_test.go — interrupt scenario test
```

### queue.go

Implement `FrameQueue` with:
- `Put(frame Frame)` — blocks if at capacity (cap = 512)
- `Get(ctx context.Context) (Frame, error)` — blocks until frame available or ctx cancelled
- `HasUninterruptible() bool` — O(1), uses atomic counter
- `Reset()` — under mutex, drain all non-UninterruptibleFrame items

```go
type FrameQueue struct {
    mu       sync.Mutex
    cond     *sync.Cond
    items    []Frame
    nUninterruptible int64  // atomic
}
```

---

## Unit Tests (queue_test.go)

1. **Basic put/get**: put 10 frames, get 10 frames in FIFO order.

2. **HasUninterruptible false**: queue 5 AudioFrames → `HasUninterruptible()` == false.

3. **HasUninterruptible true**: queue 5 AudioFrames + 1 EndFrame → `HasUninterruptible()` == true.
   Dequeue EndFrame → `HasUninterruptible()` == false again.

4. **Reset preserves EndFrame**: queue [Audio, Audio, EndFrame, Audio, Audio] → call `Reset()`
   → queue contains exactly [EndFrame].

5. **Reset when empty**: no panic, no-op.

6. **Reset + Put concurrency** (run with `-race`): two goroutines putting, one calling Reset.
   No deadlock within 100 ms.

---

## Integration Test (integration_test.go)

Simulates the output transport audio task loop in isolation.

**Scenario**: "interrupt arrives mid-playback, session terminates cleanly"

1. Start a goroutine `audioTask` that reads from FrameQueue and appends to a `played []Frame` slice.
   The goroutine exits when it reads an EndFrame.

2. Feed 20 × AudioFrame + EndFrame into the queue (EndFrame last).

3. After 5 ms (while the audio task has processed ~3–5 frames), call `queue.Reset()` from a
   separate goroutine (simulating InterruptionFrame arrival). Since EndFrame is in the queue,
   `HasUninterruptible()` was true → use Reset path.

4. Feed a new EndFrame into the queue (the interrupt handler re-enqueues EndFrame after Reset).

5. Wait for the audio task goroutine to exit (timeout 500 ms).

**Assert**:
- Audio task exits (EndFrame was delivered).
- `played` contains fewer than 20 AudioFrames (some were drained by Reset).
- `played` contains exactly one EndFrame (not zero, not two).
- No goroutine leak (`runtime.NumGoroutine()` same before and after).
- Run with `-race` — no data race detected.

---

## Success Criteria

All unit tests pass. Integration test passes. `-race` clean.

---

## Measurements to Record

- Number of AudioFrames delivered before interrupt (expected: 3–8, confirms Reset drained the rest)
- Time from `Reset()` call to audioTask exit (should be < 10 ms)
- `runtime.NumGoroutine()` delta (should be 0)

---

## What Failure Looks Like

- EndFrame lost → audioTask never exits → session deadlock.
- Deadlock in `Get()` after `Reset()` empties the queue → `cond.Broadcast()` missing.
- Race condition → investigate whether `nUninterruptible` needs atomic vs mutex protection.
