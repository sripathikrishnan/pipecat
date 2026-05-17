# EXP-001: Output Transport End-to-End

**Risk addressed**: The full output transport / MediaSender is the highest-risk component in
Gato. This experiment validates every concern in isolation before building the framework:
real-time pacing, chunk normalization, BotStarted/Stopped state machine, interruption
correctness, and EndFrame survival.

**Status**: [ ]

**Depends on**: nothing

---

## Why This is the Hardest Part

Pipecat's `BaseOutputTransport.MediaSender` (600+ Python lines) does all of the following
concurrently:

1. **Chunk normalization** — TTS delivers audio in large variable-size blobs (often 0.5–2 sec).
   These must be re-chunked into fixed 10 ms pieces before queuing. Without this, you cannot
   interrupt mid-chunk; the bot will speak for up to 2 seconds after the user starts talking.

2. **Real-time pacing** — pipecat + aiortc is pull-based: WebRTC calls `recv()` at the right time.
   Pion is push-based: we call `WriteSample()`. We must pace ourselves by sleeping 10 ms
   between each 10 ms chunk. Getting this wrong means either: audio arrives at the browser
   faster than realtime (jitter buffer absorbs it, but interrupt latency is undefined), or
   slower than realtime (audible gaps).

3. **BotStarted/Stopped state machine** — the output transport knows when the bot is speaking
   by detecting non-silence TTS audio. It pushes `BotStartedSpeakingFrame` and
   `BotStoppedSpeakingFrame` in *both directions* (downstream and upstream). The upstream
   signal is how the assistant aggregator knows to finalize the turn. Getting the direction
   wrong silently breaks turn tracking — no crash, just wrong behavior.

4. **Interruption** — `InterruptionFrame` arrives while audio is playing. The audio task must
   stop after the current 10 ms chunk. If an `EndFrame` was queued behind the audio,
   it must survive the interruption and still be delivered (otherwise the session never shuts
   down cleanly).

5. **Resume** — after an interrupt, new TTS audio should flow without restarting the Pion
   peer connection. The audio task must be restartable.

---

## Program

```
experiments/gato/experiments/exp-001/
  main.go          — HTTP server + Pion PeerConnection
  output.go        — OutputTransport implementation under test
  output_test.go   — unit tests for state machine + interruption
  index.html       — browser test page (WebRTC, interrupt button, latency display)
  testdata/
    tts_chunk_small.raw  — 100 ms of speech, 48 kHz mono (simulates small TTS chunk)
    tts_chunk_large.raw  — 2000 ms of speech, 48 kHz mono (simulates large TTS chunk)
    silence.raw          — 500 ms of silence, 48 kHz mono
```

`output.go` implements the MediaSender in isolation:
- Reads from a `FrameQueue` (from EXP-003 once available; use a channel-backed stub here).
- Writes to a Pion `TrackLocalStaticSample`.
- Runs the audio task goroutine with 10 ms chunk / sleep pacing.
- Implements `handleInterrupt()` and `handleEndFrame()`.

---

## Scenario 1 — Smooth playback with large TTS chunks

Feed `tts_chunk_large.raw` (2000 ms blob) as a single frame. The output transport must:
1. Re-chunk it into 200 × 10 ms pieces.
2. Play each with a 10 ms sleep between them.
3. Total playback duration measured at browser: 2000 ± 50 ms.

**Assert**: browser `AudioContext.currentTime` advances 2 seconds ± 5% from first to last
audio sample.

---

## Scenario 2 — Interrupt mid-playback, measure latency

1. Start feeding `tts_chunk_large.raw` (2000 ms).
2. After 300 ms of realtime, send `InterruptionFrame`.
3. Measure time from interrupt signal to audio stop at browser.

**Assert**: audio stops within 20 ms of interrupt signal (one chunk boundary).
Run 10 times, record p50 / p99.

---

## Scenario 3 — EndFrame survives interruption

1. Enqueue: [500 ms audio] [500 ms audio] [EndFrame] [500 ms audio]
2. After 100 ms, send `InterruptionFrame`.
3. **Assert**:
   - Audio task stops early (does not play all 1500 ms of audio).
   - EndFrame is still delivered to the downstream handler.
   - Session goroutines terminate cleanly within 200 ms.
4. Run with `-race`.

---

## Scenario 4 — BotStarted/Stopped direction

1. Feed audio → confirm `BotStartedSpeakingFrame` is pushed both downstream AND upstream.
2. Audio ends (via `TTSStoppedFrame`) → confirm `BotStoppedSpeakingFrame` both directions.
3. Feed silence-only audio → confirm no `BotStartedSpeakingFrame` (silence should not trigger).

**Assert**: frame direction is correct by intercepting push calls with a recording wrapper.

---

## Scenario 5 — Resume after interrupt

1. Feed 500 ms audio, interrupt at 200 ms.
2. Immediately feed a new 500 ms audio chunk.
3. **Assert**: new audio plays correctly without restarting the peer connection.
4. Confirm `BotStartedSpeakingFrame` fires again for the new audio.

---

## Success Criteria

- Scenario 1: playback duration within 5% of expected.
- Scenario 2: p99 interrupt latency ≤ 20 ms across 10 trials.
- Scenario 3: EndFrame delivered; no goroutine leak; `-race` clean.
- Scenario 4: both directions confirmed for BotStarted and BotStopped.
- Scenario 5: resume works; no peer connection restart required.

---

## Measurements to Record

- Interrupt latency p50 / p99 (Scenario 2, 10 trials)
- Playback timing accuracy (Scenario 1, % deviation from expected)
- Goroutine count delta after Scenario 3
- Whether Pion's internal jitter buffer adds observable startup delay (subjective, Scenario 1)

---

## What Failure Looks Like

| Symptom | Likely cause |
|---------|--------------|
| Glitchy audio | Chunk size mismatch — verify `WriteSample` duration matches actual audio length |
| Interrupt latency > 50 ms | Sleep not between chunks, or Pion has internal buffering — try smaller chunk size (5 ms) |
| EndFrame lost | `FrameQueue.Reset()` not implemented yet — stub it as "drain all" for now and fix before EXP-003 |
| BotStopped fires twice | State machine doesn't guard on `_bot_speaking` flag being already false |
| Resume doesn't play audio | Audio task goroutine not restarted after cancel — check goroutine lifecycle |
