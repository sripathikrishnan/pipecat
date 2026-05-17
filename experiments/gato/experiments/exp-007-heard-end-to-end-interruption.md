# EXP-007: [HEARD] End-to-End Interruption

**Risk addressed**: Does the interruption path correctly report exact heard_text — the text
whose audio completed playout before the interrupt signal fired?

**Status**: [ ]

**Depends on**: EXP-001 (Pion pacing), EXP-002 (sample rate), EXP-003 (interrupt-safe queue)

---

## Hypothesis

The [HEARD] mechanism (from pipecat-adk) sends `TurnInterrupted(heard_text)` to the business
layer, where `heard_text` is the exact TTS text that fully played out before the interrupt.
This lets the business layer know what the user heard, so it doesn't repeat it.

Our implementation: each `TTSAudioRawFrame` carries the text segment it was derived from.
The audio task goroutine marks a text segment as "heard" after writing the corresponding audio
to the Pion track. On interruption, the accumulated heard text is the correct value.

The unknown: does the text-segment → audio-chunk mapping survive chunking and queue draining?
A 1-second TTS response may be split into 100 × 10 ms audio chunks. The "heard" marker must
be associated with the *last* chunk of a text segment, not the first.

---

## Program

Standalone — does not require a browser or business layer. Uses a test harness.

```
experiments/gato/experiments/exp-007/
  main.go           — test harness
  output_transport.go — minimal output transport implementation (from EXP-001 + EXP-003)
  heard_test.go     — HEARD accuracy tests
```

### Test Harness

1. A `FakeTTSProvider` produces pre-segmented audio:
   - Segment 1: text="Hello world", audio = 500 ms sine wave
   - Segment 2: text="How are you", audio = 500 ms sine wave
   - Segment 3: text="today", audio = 300 ms sine wave

2. The `OutputTransport` (using EXP-003's queue) runs the audio task.
   Instead of writing to a Pion track, it writes to a local counter (`playedMs`).

3. An interrupt goroutine fires after a configurable delay (`interruptAfterMs`).
   It sends an `InterruptionFrame` into the transport and reads back `heard_text`.

4. Test verifies `heard_text` equals the expected string for each `interruptAfterMs` value.

---

## Test Cases (heard_test.go)

| interruptAfterMs | Expected heard_text      | Reasoning                              |
|------------------|--------------------------|----------------------------------------|
| 0                | ""                       | Interrupt before any audio sent        |
| 200              | ""                       | Still in segment 1, not yet complete   |
| 600              | "Hello world"            | Segment 1 complete (500 ms), in seg 2  |
| 1100             | "Hello world How are you"| Both segments complete, in seg 3       |
| 1500             | "Hello world How are you today" | All segments complete              |

Run each case 10 times to catch timing non-determinism.

---

## Implementation Details

### Text-segment tracking

Each TTS text segment generates N audio chunks. The `heard_text` update fires when the last
chunk of a segment is dequeued from the FrameQueue (not when it is queued).

```go
// Inside audio task goroutine:
chunk, segmentText, isLastChunk := queue.Get()
writeToTrack(chunk)
if isLastChunk && segmentText != "" {
    heardText += segmentText + " "
}
```

### Interruption handler

```go
func (t *OutputTransport) handleInterrupt() string {
    // Signal audio task to stop after current chunk.
    atomic.StoreInt32(&t.interrupted, 1)
    // Wait for audio task to acknowledge.
    <-t.interruptAck
    // Return accumulated heard text (trimmed).
    return strings.TrimSpace(t.heardText)
}
```

The audio task checks `atomic.LoadInt32(&t.interrupted)` between every chunk and breaks
the loop when set.

---

## Success Criteria

1. All 5 test cases pass across 10 runs each (50 total).
2. No case where `heard_text` includes a segment that had not fully played out.
3. No case where `heard_text` is missing a segment that had fully played out.
4. `-race` clean.

---

## Measurements to Record

- Timing accuracy: how many ms off from `interruptAfterMs` does the interrupt actually fire?
  (The audio task sleeps 10 ms per chunk, so granularity is ±10 ms.)
- Any non-deterministic failures across the 50 runs (record percentage).

---

## What Failure Looks Like

- heard_text includes a segment the user did not hear → the "last chunk" marker fires too
  early. Check whether `isLastChunk` is set on enqueue (wrong) vs dequeue (correct).

- heard_text is empty when it should contain segment 1 → the interrupt fires between
  `writeToTrack()` and the `heardText` update. Fix: update `heardText` before `writeToTrack()`.

- Non-deterministic failures across runs → timing race. Add a brief `time.Sleep(5ms)` after
  the interrupt signal to allow the audio task to finish its current chunk.
