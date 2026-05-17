# EXP-004: Silero VAD via CGO/ONNX

**Risk addressed**: Can Silero VAD run via CGO at 20 ms chunk cadence with acceptable inference
latency across multiple concurrent sessions? Does the goroutine pool model work?

**Status**: [ ]

**Depends on**: nothing (fully standalone)

---

## Hypothesis

Silero VAD is a lightweight LSTM model (~2 MB). Via `onnxruntime-go` (CGO wrapper), one
shared ONNX session can be called concurrently from a goroutine pool sized to `runtime.NumCPU()`.

Inference per 20 ms chunk should be well under 5 ms on modern hardware (budget is 20 ms per
chunk; inference consuming < 25% is acceptable).

The tricky part: onnxruntime sessions maintain per-request state (LSTM hidden state for Silero).
We must understand whether one session handles concurrent calls or whether we need N sessions.
The onnxruntime C API is thread-safe for inference but state must be managed per-stream.

---

## Program

```
experiments/gato/experiments/exp-004/
  main.go         — goroutine pool + benchmark
  testdata/
    silero_vad.onnx  — Silero VAD model (download separately)
    speech.raw       — real speech PCM, 16 kHz mono (10 seconds)
    silence.raw      — silence PCM, 16 kHz mono (5 seconds)
```

`main.go` runs three scenarios:

**Scenario 1 — Single stream**: 1 goroutine processing 500 chunks (10 seconds at 20 ms / chunk).
Record inference latency per chunk. Verify VAD output (1 = speech, 0 = silence) is correct.

**Scenario 2 — Concurrent streams**: N goroutines (N = 1, 10, 50, 100) each processing the
same audio file simultaneously. Measure p50/p99 inference latency per chunk per goroutine.

**Scenario 3 — LSTM state isolation**: process `speech.raw` then `silence.raw` on the same
goroutine without resetting state. Verify that the transition is detected correctly. Then
process a second goroutine's stream interleaved with the first to confirm state doesn't leak
between streams.

---

## ONNX Session Strategy

Silero's LSTM requires per-stream hidden state (`h` and `c` tensors). Two strategies:

**Strategy A — One session per logical stream**: each concurrent session has its own
onnxruntime session. Thread-safe, fully isolated, but N sessions × model size memory.

**Strategy B — Shared session, external state**: one session shared across all streams;
caller passes hidden state as input tensors and receives updated state as output. Silero's
ONNX export supports this (the model has `h` and `c` as I/O tensors). Memory efficient;
requires caller to maintain state.

Test both strategies. Record memory usage and latency for each.

---

## Success Criteria

1. **Correctness**: VAD correctly detects speech/silence boundaries in `testdata/speech.raw`
   (known ground truth from pipecat's Silero integration).

2. **Latency (single stream)**: p99 inference latency < 5 ms per 20 ms chunk.

3. **Latency (50 concurrent)**: p99 inference latency < 10 ms per chunk
   (still well within the 20 ms budget).

4. **State isolation**: interleaved streams do not corrupt each other's LSTM state.

5. **Memory**: Strategy B (shared session) uses < 50 MB for 100 concurrent streams.

---

## Measurements to Record

- ONNX session strategy chosen (A or B) and why
- p50 / p99 inference latency at 1, 10, 50, 100 concurrent streams
- Memory usage at 100 concurrent streams
- Whether CGO build succeeds with `libonnxruntime.so` on the target OS/arch
- Build time with CGO enabled (relevant for CI)

---

## What Failure Looks Like

- Inference latency > 20 ms at 50 concurrent → goroutine pool is too small or ONNX session
  is serialised internally. Solution: benchmark pool sizes; try increasing `NumCPU()` multiplier.
- LSTM state leaks between streams → must use Strategy A or reset state between runs.
- CGO build fails on cross-compilation target → document the Docker build requirements here.

---

## Setup Notes

Download Silero VAD ONNX model:
```bash
# silero_vad v5 (recommended — smaller, faster than v4)
curl -L https://raw.githubusercontent.com/snakers4/silero-vad/master/src/silero_vad/data/silero_vad.onnx \
     -o testdata/silero_vad.onnx
```

Install onnxruntime shared library (required for CGO build):
```bash
# macOS
brew install onnxruntime

# Linux
apt-get install libonnxruntime-dev
# or download from https://github.com/microsoft/onnxruntime/releases
```
