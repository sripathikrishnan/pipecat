# EXP-002: Audio Resampling for 24 kHz → 48 kHz

**Risk addressed**: Browser/Opus requires 48 kHz. Google TTS outputs at 24 kHz. A streaming
resampler is mandatory. This experiment finds the most efficient Go implementation.

**Status**: [ ]

**Depends on**: EXP-001 (Pion setup, confirms 48 kHz requirement)

---

## The Problem

24 kHz → 48 kHz is a 2:1 integer upsampling ratio — the simplest possible case. Every input
sample maps to 2 output samples. No fractional interpolation required.

The resampler must be:
- **Stateful / streaming** — TTS chunks arrive in arbitrary sizes (100–2000 ms). The resampler
  processes them incrementally, not in one batch.
- **Low allocation** — called 100× per second per session. No heap allocation per chunk.
- **Zero CGO overhead on the hot path** — CGO calls have ~5–50 ns of overhead per call.
  For 10 ms chunks this is negligible, but it pins goroutines to OS threads.

We already use CGO for ONNX (VAD, TurnDetect). Adding libsamplerate CGO is not a new
dependency *category*, but it is a new build-time dependency and complicates cross-compilation.

---

## Three Implementations to Benchmark

### Implementation A — Linear interpolation (pure Go, ~30 lines)

For 2:1 upsampling, insert one interpolated sample between each pair of adjacent input samples:

```go
// output[2i]   = input[i]
// output[2i+1] = (input[i] + input[i+1]) / 2
```

Streaming state: one sample carried over between chunks (the last sample of the previous chunk
is needed to interpolate the first sample of the next chunk).

Quality: no aliasing (upsampling only). SNR ≈ 40 dB on pure tones. Adequate for voice.

### Implementation B — Polyphase FIR (pure Go, ~150 lines)

A 4-tap polyphase FIR filter with pre-computed coefficients for 2:1 upsampling. Reduces
imaging artifacts vs linear interpolation. Higher CPU per sample but still pure Go.

Streaming state: filter delay line (4 samples).

Quality: SNR ≈ 80 dB. Imperceptible to human hearing.

### Implementation C — libsamplerate via CGO

Use `github.com/dh1tw/gosamplerate` (wraps `libsamplerate` by Erik de Castro Lopo).
`SRC_SINC_FASTEST` quality setting. Considered the reference implementation for audio resampling.

Streaming state: managed by libsamplerate internally. Must call `src_reset()` on interruption.

Quality: SNR ≈ 100+ dB. More than needed for voice.

---

## Program

```
experiments/gato/experiments/exp-002/
  resampler_linear.go     — Implementation A
  resampler_fir.go        — Implementation B
  resampler_libsrc.go     — Implementation C (build tag: cgo)
  resampler_bench_test.go — benchmarks + quality tests
  testdata/
    sine_24k.raw   — 440 Hz pure tone, 24 kHz mono, 10 seconds
    speech_24k.raw — real speech, 24 kHz mono, 30 seconds
```

---

## Tests

### Correctness (all three implementations)

1. Feed `sine_24k.raw` (440 Hz) through each resampler → output must be 48 kHz.
2. Verify pitch is still 440 Hz: FFT the output, find peak frequency.
3. Measure SNR: compare resampled output against a reference 440 Hz 48 kHz sine wave.
   - Implementation A must have SNR ≥ 30 dB.
   - Implementation B must have SNR ≥ 60 dB.
   - Implementation C must have SNR ≥ 90 dB.

### Streaming correctness

1. Feed `speech_24k.raw` through each resampler in chunks of:
   - 20 ms (400 samples) — the VAD chunk size
   - 100 ms (2000 samples) — a typical TTS mini-chunk
   - 2000 ms (40000 samples) — a large TTS response
2. Compare chunked output vs whole-file output sample by sample.
   **Assert**: results are identical (stateful resampler matches batch resampler).

### Interruption / reset

1. Process 500 ms of audio.
2. Call `Reset()` (simulating an interruption).
3. Process another 500 ms of audio.
4. **Assert**: no artifacts at the reset boundary (first 10 ms of post-reset audio is clean).

---

## Benchmarks

```go
// go test -bench=. -benchmem -count=5
BenchmarkLinear_10ms    // 10 ms chunk at 24 kHz = 240 samples → 480 samples out
BenchmarkFIR_10ms
BenchmarkLibSRC_10ms

BenchmarkLinear_100ms   // 100 ms chunk
BenchmarkFIR_100ms
BenchmarkLibSRC_100ms
```

Target: all implementations must process a 10 ms chunk in < 50 µs
(budget: 10 ms realtime / 200 sessions per process = 50 µs per call).

---

## Decision Criteria

| Criterion | Weight | A (Linear) | B (FIR) | C (libsrc) |
|-----------|--------|------------|---------|------------|
| SNR adequate for voice (≥ 30 dB) | Must-have | ✓ | ✓ | ✓ |
| Zero CGO on hot path | High | ✓ | ✓ | — |
| < 50 µs for 10 ms chunk | Must-have | measure | measure | measure |
| Implementation complexity | Medium | low | medium | low |
| Cross-compilation | Medium | trivial | trivial | requires .so |

**Default choice: Implementation A** unless benchmarks show it cannot meet the 50 µs budget
at 200 concurrent sessions, or if speech quality tests reveal audible artifacts on real speech.

---

## Measurements to Record

For each implementation:
- SNR on 440 Hz pure tone (dB)
- ns/op for 10 ms chunk (from Go benchmark)
- ns/op for 100 ms chunk
- Whether streaming correctness test passes
- Audible quality on `speech_24k.raw` (subjective: any harshness or ringing?)
- CGO or not (affects cross-compilation)

---

## What Failure Looks Like

- Implementation A SNR < 30 dB → investigate; 2:1 linear upsampling should not be this bad.
  Check integer overflow in the interpolation arithmetic.
- Benchmark > 50 µs → all three implementations should be well under this for 2:1 upsampling.
  If not, check for unexpected allocations (`-benchmem`, expect 0 alloc/op on hot path).
- Streaming mismatch → carry-over state is wrong. Check the boundary sample handling.
