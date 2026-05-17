# EXP-007: [HEARD] End-to-End Interruption — Results

**Date**: 2026-05-17
**Status**: [x] COMPLETE — All tests pass. Architecture validated.

---

## Results

### Summary Test (5 cases × 1 run)

| Case | Elapsed | playedMs | heardText | Result |
|------|---------|----------|-----------|--------|
| interrupt@0ms (before any chunk) | 0ms | 0 | `""` | PASS |
| interrupt@200ms (mid seg1) | 203ms | 180 | `""` | PASS |
| interrupt after seg1 done | 573ms | 500 | `"Hello world"` | PASS |
| interrupt after seg2 done | 1145ms | 1000 | `"Hello world How are you"` | PASS |
| interrupt after all done | 1478ms | 1300 | `"Hello world How are you today"` | PASS |

### Parallel Tests (5 cases × 10 runs each, -race)

| Test | Runs | Result |
|------|------|--------|
| TestHEARD_0ms | 10 | 10/10 PASS |
| TestHEARD_MidSeg1 | 10 | 10/10 PASS |
| TestHEARD_AfterSeg1 | 10 | 10/10 PASS |
| TestHEARD_AfterSeg2 | 10 | 10/10 PASS |
| TestHEARD_AfterAll | 10 | 10/10 PASS |

All tests pass with `-race` flag. Zero goroutine leaks.

---

## Key Findings

### 1. The [HEARD] mechanism is correct

Marking a segment as "heard" when the **last chunk is dequeued** (not enqueued) produces
the correct result in all cases. The critical ordering is:

```go
// Audio task, per chunk:
t.playedMs.Add(...)              // "write" to track
if item.isLastChunk {
    t.heardText += item.segmentText + " "  // update AFTER write
    t.segmentDone <- item.segmentText       // signal test
}
time.Sleep(chunkDuration)        // pace to real time
```

The interrupt check is at the top of the next loop iteration, so the audio task
finishes the current chunk (including the `heardText` update) before stopping.

### 2. Timing accuracy: ~11ms per chunk on macOS arm64

The 10ms sleep paces at approximately **11ms per chunk** on macOS arm64, not 10ms.
This is consistent with macOS's minimum timer resolution rounding up.

| Expected chunk time | Measured (avg) |
|---------------------|----------------|
| 10ms | ~11ms |
| 500ms audio | ~573ms wall-clock |
| 1000ms audio | ~1145ms wall-clock |
| 1300ms audio | ~1478ms wall-clock |

This 10% overhead is consistent with EXP-001's finding of 1.5% timer error
(EXP-001 measured larger blobs where errors average out more; EXP-007's 10ms
individual chunks accumulate the rounding error per chunk).

**Important implication**: The initial test using wall-clock timing (interrupt at 1100ms
expecting segment 2 complete by 1000ms audio time) failed because the audio task
needed ~1145ms wall clock. Tests must not assume 1:1 audio-time to wall-clock ratio.

### 3. Wall-clock timing is unreliable for segment boundary tests

The first implementation used `time.Sleep(interruptAfterMs)` for all test cases.
The 1100ms case failed because it landed exactly at the segment 2 boundary (1145ms actual).

**Fix**: Use `segmentDone chan string` for precise synchronization — the audio task signals
after each segment completes. Tests waiting for segment N are completely deterministic.

Wall-clock timing is still valid for "mid-segment" tests where the interrupt fires
well before the segment boundary (200ms with a 350ms margin to segment 1 end).

### 4. handleInterrupt() is safe after audio task exit

When all audio drains before the interrupt fires (the 1478ms case above), the audio task
exits cleanly (queue closed → channel receive returns false → `close(t.done)`). 
`handleInterrupt()` handles this via:

```go
select {
case <-t.interruptAck: // audio task was still running
case <-t.done:         // audio task already exited
}
```

The `heardText` field is safe to read because:
- Audio task writes `heardText` before closing `done`
- `handleInterrupt` reads `heardText` after receiving from `done`
- This is a valid happens-before chain under Go's memory model.

---

## Architecture Decision

**The [HEARD] mechanism is validated and production-ready.**

Implementation requirements for Gato:
1. Each `TTSAudioFrame` must carry the text of its origin segment (`segmentText`).
2. Only the last chunk of each segment carries a non-empty `segmentText`.
3. The audio task updates `heardText` after "writing" the chunk, before sleeping.
4. The interrupted flag is checked at the top of each iteration (after the sleep).
5. `handleInterrupt()` waits on either `interruptAck` or `done` to handle both cases.

---

## Timing Implication for Production

The 10% pacing overhead (11ms per chunk instead of 10ms) means audio plays out
~10% slower than the TTS-reported duration. For a 1-second TTS segment:
- Expected: 1000ms wall-clock
- Actual: ~1100ms wall-clock

This should be acceptable for voice agents where the exact playback duration
is not critical. If precision is required, use a monotonic clock target instead
of cumulative `time.Sleep`:

```go
target := time.Now()
for each chunk {
    target = target.Add(chunkDuration)
    // write chunk
    time.Sleep(time.Until(target))  // self-correcting, doesn't accumulate drift
}
```

This was not implemented in EXP-007 (out of scope) but should be considered for EXP-008.
