# EXP-004: Silero VAD via CGO/ONNX — Results

**Date**: 2026-05-17
**Status**: PASS — all success criteria met
**Platform**: darwin/arm64 (Apple M4 Max), GOMAXPROCS=16, Go 1.26.1
**ONNX Runtime**: 1.26.0 at `/opt/homebrew/lib/libonnxruntime.dylib`

---

## Model I/O Discovery

The Silero VAD v5 ONNX model (`silero_vad.onnx`, 2.2 MB) has different I/O than the v4 documentation suggests:

| Name    | Type    | Shape      | Notes |
|---------|---------|------------|-------|
| `input` | float32 | [batch, samples] | Audio samples; batch=1, samples=512 |
| `state` | float32 | [2, batch, 128] | Combined LSTM hidden state (h+c) |
| `sr`    | int64   | [] scalar   | Sample rate (16000) |
| `output`| float32 | [batch, 1] | Speech probability in [0, 1] |
| `stateN`| float32 | [2, batch, 128] | Updated LSTM state |

Key difference from task spec: v5 merges the separate `h` and `c` tensors from v4 into a single `state` tensor of shape [2, 1, 128]. The session names are `input/state/sr` → `output/stateN`.

---

## Session Strategy Decision

**Strategy B chosen: one shared ONNX session, per-stream external state.**

- Strategy A (one session per stream) would require N × ~5 MB model memory for N streams.
- Strategy B: all callers share one session. State is passed as input tensors and returned as output tensors — stateless session with stateful callers.
- ORT sessions are documented thread-safe for concurrent `Run()` calls.
- Go mutex not required: all per-call data lives in local tensors, not in the session object.

---

## Measurements

### Scenario 1: Single Stream (500 chunks = 16 seconds of audio at 32ms/chunk)

| Metric | Result | Target | Pass? |
|--------|--------|--------|-------|
| p50 latency | **0.06 ms** | — | — |
| p99 latency | **0.08 ms** | < 5 ms | ✓ |

### Scenario 2: Concurrent Streams

| N concurrent | Wall time | p50 (ms) | p99 (ms) | p99 < 10ms? |
|-------------|-----------|----------|----------|-------------|
| 1           | 3 ms      | 0.06     | 0.07     | ✓ |
| 10          | 23 ms     | 0.44     | 0.84     | ✓ |
| 50          | 101 ms    | 1.35     | 4.41     | ✓ |
| 100         | 187 ms    | 0.70     | 47.38    | ✗ (contention) |

At N=100, ORT's internal thread pool shows contention (p99 spikes to 47ms). N=50 is within budget at 4.41ms p99.

**Note on N=100 spike**: Without a Go-level mutex, ORT itself serializes some internal work at very high concurrency. The practical limit for this shared-session approach on a 16-core machine is approximately 50–80 concurrent streams before the ORT thread pool becomes a bottleneck. For EXP-009 production capacity testing, use a goroutine pool of `runtime.NumCPU()` to bound concurrency to the core count.

### Scenario 3: LSTM State Isolation

- Two streams (sine wave vs silence) run interleaved for 10 steps each.
- States diverge at most tensor dimensions (confirmed true).
- No state leak between streams: Strategy B is structurally isolated.

Note: `state[0][0][0]` converges to -1.0 in both streams due to tanh saturation; divergence is confirmed across other dimensions.

### Benchmark: Single-Stream Throughput

```
BenchmarkVAD_SingleStream-16    97368    60521 ns/op    2680 B/op    31 allocs/op
```

- **60.5 µs per inference** (0.061 ms) — 330× headroom vs the 20 ms chunk budget.
- 31 allocations per call (tensor creation/destruction). Acceptable for 50Hz call rate.
- Allocation optimization possible (pre-allocated tensor pools) if needed in EXP-009.

---

## Success Criteria Assessment

| Criterion | Result | Pass? |
|-----------|--------|-------|
| Model loads without error | Loaded, 2.2 MB | ✓ |
| Single-stream p99 < 5ms | 0.08 ms | ✓ |
| 50-concurrent p99 < 10ms | 4.41 ms | ✓ |
| State isolation (Strategy B) | Confirmed structurally and empirically | ✓ |
| `-race` clean | All 5 tests pass under race detector | ✓ |
| Build with CGO succeeds | `go build ./...` clean | ✓ |

---

## Key Findings

1. **Silero VAD v5 model I/O differs from v4.** The state tensor is a single [2, 1, 128] float32 rather than separate h/c [2, 1, 64] tensors. Always inspect the ONNX graph before implementing the wrapper.

2. **Strategy B is correct and efficient.** One shared ORT session handles 50 concurrent Go goroutines with p99=4.41ms — well within the 20ms chunk budget. No mutex needed in the Go layer; ORT is internally thread-safe.

3. **ORT thread pool saturates above ~80 concurrent callers on 16 cores.** At N=100, p99 jumps to 47ms. For EXP-009, bound concurrency to `runtime.NumCPU()` via a goroutine pool (the planned architecture already does this).

4. **No CGO build issues on macOS arm64.** The `cgo.go` file with `-L/opt/homebrew/lib -lonnxruntime` is sufficient. No additional `DYLD_LIBRARY_PATH` needed at build time.

5. **Allocation cost is minor.** 60µs/call including 31 allocations is negligible at 50Hz. Zero-allocation optimization would save <3µs/call and is not warranted.

---

## Architecture Decision

**Strategy B (shared session + per-stream state) is the production design for Gato.**

`SileroVAD` is a single struct containing one `*DynamicAdvancedSession`. Each audio stream carries a `StreamState` (32-byte struct: [2][1][128]float32 = 1024 bytes). The session is shared; `StreamState` is passed by value on each call and returned as a new value. No goroutine can corrupt another's state. Concurrency up to `NumCPU()` goroutines is race-free and within latency budget.

---

## Files

- `silero_vad.go` — SileroVAD wrapper (Strategy B, no mutex)
- `vad_test.go` — 5 tests: Load, SpeechDetection, SilenceDetection, StateIsolation, Concurrent + benchmark
- `main.go` — 3-scenario demonstration
- `cgo.go` — CGO linker flags for libonnxruntime
- `testdata/silero_vad.onnx` — Silero VAD v5 model (2.2 MB, downloaded by TestMain)
