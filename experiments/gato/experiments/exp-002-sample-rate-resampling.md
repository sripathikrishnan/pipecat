# EXP-002: Sample Rate & Resampling

**Risk addressed**: Does Pion's Opus encoder accept audio at 24 kHz (Google TTS output rate)?
If not, we need a streaming resampler — and we need to choose an implementation before building
the output transport.

**Status**: [ ]

**Depends on**: EXP-001 (Pion peer connection setup)

---

## Hypothesis

Google Cloud TTS outputs audio at a configurable sample rate (8, 16, 22050, or 24000 Hz).
Pion's Opus encoder is typically configured at 48 kHz internally. If we feed it 24 kHz audio
with correct metadata, it may accept and resample internally, or it may produce distorted output.

We want to find out:
1. What sample rate must we pass to Pion's `WriteSample()`?
2. If we must resample, what is the minimum acceptable Go implementation?

---

## Program

Extends EXP-001's setup. Reuse the HTTP server and peer connection code.

```
experiments/gato/experiments/exp-002/
  main.go       — same structure as EXP-001 but tests multiple sample rates
  testdata/
    sine_16k.raw   — 440 Hz sine, 16 kHz mono, 5 seconds
    sine_24k.raw   — 440 Hz sine, 24 kHz mono, 5 seconds
    sine_48k.raw   — 440 Hz sine, 48 kHz mono, 5 seconds
```

`main.go` runs three trials in sequence, pausing 2 seconds between each. The test page
shows which trial is active and allows subjective quality rating.

---

## Trials

**Trial 1**: Feed `sine_16k.raw` via `TrackLocalStaticSample` configured for 16 kHz.
**Trial 2**: Feed `sine_24k.raw` via `TrackLocalStaticSample` configured for 24 kHz.
**Trial 3**: Feed `sine_48k.raw` via `TrackLocalStaticSample` configured for 48 kHz.

For each trial, record:
- Does audio play? (y/n)
- Is the pitch correct? (440 Hz → correct; otherwise resampling is wrong)
- Any distortion or artifacts? (subjective)

---

## Success Criteria

**Best case**: Trial 2 (24 kHz) plays correctly. No resampler needed. Google TTS configured
to output 24 kHz → Pion configured to expect 24 kHz → done.

**Acceptable**: Trial 3 only works. We must resample 24 kHz → 48 kHz. Go to Resampler Decision
below.

**Failure**: None of the trials produce clean audio. Pion codec setup is wrong; investigate
`opus.Options` and `webrtc.RTPCodecCapability` before proceeding.

---

## Resampler Decision (if needed)

If resampling is required, evaluate two options and pick one before proceeding to EXP-003:

**Option A — Linear interpolation (pure Go, ~100 lines)**
Write a stateful resampler that upsamples 24 kHz → 48 kHz by linear interpolation.
Quality is sufficient for voice (not music).
Test: feed `sine_24k.raw` through the resampler and verify 440 Hz output.

**Option B — CGO-wrap libsamplerate**
Use `github.com/dh1tw/gosamplerate` or equivalent CGO binding.
Higher quality but adds a second CGO dependency alongside ONNX.
Acceptable only if Option A produces audible artifacts in real speech.

Record the decision and the rationale in the Results section.

---

## Measurements to Record

- Which sample rate trial succeeded
- Whether a resampler is needed (y/n)
- If yes: which option chosen and why
- Resampler latency: time to process 1 second of audio (should be < 1 ms)
