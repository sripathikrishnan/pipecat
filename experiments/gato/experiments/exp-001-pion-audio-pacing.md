# EXP-001: Pion Audio Pacing

**Risk addressed**: Output transport clock model — can we push audio to a browser via Pion
at real-time pace with smooth playback and bounded interrupt latency?

**Status**: [ ]

**Depends on**: nothing

---

## Hypothesis

In pipecat + aiortc, the WebRTC stack *pulls* audio by calling `recv()` at the right time.
Pion uses a *push* model: we call `track.WriteSample()`. If we push all audio immediately,
the client receives it ahead of time and interrupt latency becomes uncontrolled.

We believe: sleeping 10 ms between each 10 ms chunk in the sender goroutine keeps the sender
one chunk ahead of playback, bounding interrupt latency to ~10 ms.

---

## Program

Standalone Go program. No Gato packages.

```
experiments/gato/experiments/exp-001/
  main.go       — HTTP server + Pion PeerConnection
  index.html    — minimal browser test page (WebRTC offer/answer, plays audio)
  testdata/     — raw PCM test files (16 kHz mono, 24 kHz mono)
```

`main.go` does exactly three things:
1. Accepts a WebRTC offer from the browser (HTTP endpoint `/offer`).
2. Adds a `TrackLocalStaticSample` audio track to the peer connection.
3. Reads raw PCM from `testdata/speech.raw` and pushes it in a goroutine,
   sleeping 10 ms between each 10 ms chunk.

An HTTP endpoint `/interrupt` signals the sender goroutine to stop immediately.

---

## Success Criteria

1. **Smooth playback**: browser plays the test audio with no audible glitches, clicks, or
   pauses when fed at the 10 ms / sleep pace.

2. **Interrupt latency ≤ 20 ms**: time from `GET /interrupt` to when the browser's audio
   output visibly stops (measurable via browser `AudioContext.currentTime` in the test page).

3. **Clean termination**: after interrupt, the sender goroutine exits cleanly (no goroutine
   leak confirmed by `runtime.NumGoroutine()` before and after).

4. **Resume works**: after an interrupt, feeding new audio starts playback correctly
   without restarting the peer connection.

---

## Measurements to Record

- Round-trip latency from interrupt signal to audio stop (measure 5 times, record p50 / p99)
- `runtime.NumGoroutine()` before session, during playback, after interrupt
- Whether Pion's jitter buffer causes audible delay on playback start (subjective)
- CPU usage of the sender goroutine at 10 ms / sleep cadence (should be ~0%)

---

## Variants to Try

**Variant A** (baseline): Sleep exactly 10 ms between chunks.

**Variant B**: No sleep — push all chunks at once, rely on RTP timestamps. Compare interrupt
latency to Variant A. This tells us whether RTP timestamps alone are sufficient or the sleep
is essential.

**Variant C**: Push in 20 ms chunks (2× larger). Measure whether interrupt latency doubles
as expected. This validates our model.

---

## What Failure Looks Like

- Glitchy audio → Pion needs a different sample format, layout, or codec config.
- Interrupt latency > 50 ms → sleep model is wrong; investigate Pion's internal buffering.
- Goroutine leak → cancel propagation through context is broken.

If Variant B (no sleep) already gives interrupt latency < 20 ms, we can skip the sleep
and simplify the output transport significantly.

---

## Setup Notes

- Use a raw PCM file (not Opus/MP3) to eliminate codec variables.
- Use a 440 Hz sine wave for automated latency measurement (easy to detect silence onset).
- Browser test page should log `AudioContext.currentTime` when silence is detected.
- Run with `-race` flag to catch data races in the sender goroutine.
