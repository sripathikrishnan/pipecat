# EXP-005: FrameProcessor Priority Queue — Results

**Date**: 2026-05-17
**Status**: [x] COMPLETE — All tests pass. **Critical architectural finding**.

---

## Critical Finding: Nested `select` Does NOT Guarantee Priority

**Test results (initial implementation with nested `select`):**
- 4/1000 trials had SystemFrame first (0.4% success rate).

**Explanation:** When both `systemCh` and `dataCh` have items ready, Go's `select`
picks a random case. The nested-select idiom (check system channel non-blocking,
then fall to two-way select) fails when the system frame arrives DURING the
two-way select execution — Go randomly picks the data channel.

**Fix: mutex-backed priority queue.** A `priorityQueue` struct with separate
`system []Frame` and `data []Frame` slices, protected by a mutex. `pop()` always
returns from `system` first. This is DETERMINISTIC, not probabilistic.

---

## Results After Fix

| Test | Trials | Result |
|------|--------|--------|
| Priority queue (direct test) | 1000 | 1000/1000 SystemFrame first |
| Priority pipeline (push-before-start) | 100 | 100/100 SystemFrame first |
| DataFrames FIFO | 50 frames | In-order ✓ |
| SystemFrames FIFO | 10 frames | In-order ✓ |
| Cancellation goroutine leak | — | Zero leak ✓ |

All tests pass with `-race` flag.

---

## Architecture Decision

**Use mutex-backed `priorityQueue` for all FrameProcessor queues.**

The two-channel `systemCh + dataCh` + nested `select` approach CANNOT guarantee
priority. It's an antipattern in Go despite appearing idiomatic.

Pipecat uses Python's `asyncio.PriorityQueue` (a heap-based structure that always
returns the highest-priority item). The equivalent in Go is a mutex-backed struct
with separate slices for each priority tier. 

**Implementation:** ~50 lines of Go. No heap needed — we only have two priority
tiers (system and data), so two slices with simple array-backed FIFO suffice.

---

## Pipeline Priority Guarantee

Priority holds across a 2-processor pipeline IF frames are queued before goroutines
start. In production, an InterruptionFrame arrives AFTER the pipeline is running.
The guarantee is:

"If a SystemFrame is pushed to processor P, P's NEXT `pop()` call will return the
SystemFrame before any DataFrames that were in P's queue at the time of push."

DataFrames that have already been forwarded to downstream processors are NOT recalled.
This is the same guarantee pipecat provides.

---

## Architectural Implication

This finding affects the Gato FrameProcessor design. The nested-select pattern
must not be used. All processor implementations must use a priority queue.
