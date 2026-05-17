# EXP-005: FrameProcessor Priority Queue

**Risk addressed**: Does the Go FrameProcessor correctly deliver SystemFrames before queued
DataFrames, and does the two-goroutine model match pipecat's guarantees?

**Status**: [ ]

**Depends on**: nothing (pure Go, no external services)

---

## Hypothesis

Pipecat's `FrameProcessorQueue` uses Python's `PriorityQueue` to give `SystemFrame` objects
high priority over `DataFrame` objects, preserving FIFO within each tier.

In Go, the natural translation is two buffered channels plus an input goroutine that always
drains the system channel first via a nested `select`. This is idiomatic Go and avoids a
heap-based priority queue, but the nested `select` priority is only probabilistic, not strict,
under high load.

We need to verify the priority guarantee is sufficient for Gato's use case and that the
two-goroutine model (input goroutine + process goroutine) maps cleanly to pipecat's behaviour.

---

## Program

```
experiments/gato/experiments/exp-005/
  frame_processor.go   — minimal FrameProcessor implementation
  pipeline.go          — minimal Pipeline (prev/next wiring, setup/teardown)
  priority_test.go     — priority correctness tests
  ordering_test.go     — FIFO-within-tier tests
```

### Minimal FrameProcessor

Implement only the parts needed to test priority:

```go
type FrameProcessor struct {
    name       string
    systemCh   chan Frame   // high priority
    dataCh     chan Frame   // low priority
    processCh  chan Frame   // serialised output to process()
    next       *FrameProcessor
    process    func(Frame)  // user-provided handler
}
```

Two goroutines:
1. **Input goroutine**: reads from `systemCh` and `dataCh`, writes to `processCh`,
   always draining `systemCh` first.
2. **Process goroutine**: reads from `processCh`, calls `process(frame)`.

`PushFrame(frame)` routes to `systemCh` (if SystemFrame) or `dataCh` (if DataFrame).

---

## Tests

### Priority correctness (priority_test.go)

1. Create a chain of 2 processors. Processor 2 records the order frames arrive.

2. Fill processor 1's data channel to capacity (100 DataFrames) while the process goroutine
   is blocked. Then inject 1 SystemFrame (InterruptionFrame).

3. Unblock processor 1's process goroutine.

4. **Assert**: the SystemFrame arrives at processor 2 *before* any of the 100 DataFrames.

5. Run 1000 times to confirm it is not a fluke. The nested `select` must not starve
   the SystemFrame even once.

### FIFO-within-tier (ordering_test.go)

1. Push 50 DataFrames in order (IDs 1..50) to processor 1.
2. Assert processor 2 receives them in order 1..50.
3. Push 10 SystemFrames in order (IDs A..J) to processor 1.
4. Assert processor 2 receives them in order A..J (SystemFrames FIFO with each other).

### Cancellation (ordering_test.go)

1. Create a 3-processor chain. Push 50 DataFrames.
2. Cancel the context halfway through.
3. Assert processor 3 received fewer than 50 DataFrames.
4. Assert all goroutines exited cleanly (no goroutine leak).

---

## Success Criteria

1. SystemFrame always arrives before queued DataFrames across 1000 trials.
2. FIFO preserved within each tier.
3. Clean cancellation — no goroutine leaks (`runtime.NumGoroutine()` after = before).
4. All tests pass with `-race` flag.

---

## Measurements to Record

- Number of trials where SystemFrame arrived first (should be 1000/1000)
- Maximum observed lag (in # DataFrames) before SystemFrame delivered (should be 0)
- Goroutine count delta after cancellation (should be 0)

---

## What Failure Looks Like

- SystemFrame occasionally delivered after DataFrames → nested `select` priority is
  non-deterministic under this load pattern. Solution: add an explicit fan-in goroutine
  with a `select { case f := <-system: ...; default: select { case f := <-system: ...; case f := <-data: ... }}`.
  If still failing, use a heap-based priority queue (more code, strictly correct).

- Deadlock on cancellation → process goroutine is blocked on `processCh <-`; needs a
  `select` with context cancellation check.

---

## Note

This experiment does NOT implement the full FrameProcessor (observers, metrics, pause/resume,
event system). Those are implementation work, not unknowns. The only unknown being tested is
whether the priority model is correct. If this experiment passes, the full FrameProcessor
can be built with confidence.
