# EXP-006: Google Cloud STT gRPC Streaming

**Risk addressed**: Can we stream audio to Google Cloud STT and get transcriptions reliably,
including handling stream restarts (Google's 5-minute hard limit) and silence-triggered
reconnects?

**Status**: [ ]

**Depends on**: nothing (standalone, requires GCP credentials)

---

## Hypothesis

Google Cloud STT's `StreamingRecognize` is a bidirectional gRPC stream. It requires a config
message followed by audio content messages. The stream closes after 5 minutes or on long
silences, and must be transparently restarted.

In pipecat, Deepgram handles reconnect inside the service. We believe the Google STT equivalent
is straightforward in Go (gRPC is a first-class citizen), but stream restart without dropping
audio around the boundary is the unknown.

---

## Program

```
experiments/gato/experiments/exp-006/
  main.go         — standalone STT client
  testdata/
    speech.raw       — 30 seconds of speech, 16 kHz mono
    long_speech.raw  — 6 minutes of speech (crosses the 5-minute stream limit)
```

`main.go`:
1. Opens a `StreamingRecognize` stream.
2. Sends config message (16 kHz, interim results enabled, language en-US).
3. Sends audio in 100 ms chunks (not 20 ms — STT doesn't need per-VAD-chunk precision).
4. Prints interim and final transcription results with timestamps.
5. Handles stream close: buffers the last 500 ms of audio, reopens stream, resends config,
   replays buffer.

---

## Scenarios

**Scenario 1 — Basic transcription**: feed `testdata/speech.raw`. Verify:
- Interim results arrive while audio is streaming.
- Final result arrives after each sentence pause.
- Correct transcript (spot-check against known content).

**Scenario 2 — Stream restart**: feed `testdata/long_speech.raw` (6 minutes).
The Google-side close should occur around 5 minutes. Verify:
- Restart is transparent (no audio dropped at the boundary).
- Transcription continues without gap.
- Record timestamp of restart and any observed dropout.

**Scenario 3 — Silence handling**: feed 2 minutes of silence followed by 30 seconds of speech.
Verify:
- No spurious transcriptions during silence.
- Speech after the silence is transcribed correctly.
- Stream does not error out during long silence (Google may send a timeout error).

---

## Success Criteria

1. **Scenario 1**: Final transcript matches known content with WER < 10%.
2. **Scenario 2**: Stream restarts without dropping words at the boundary. Heard in audio:
   "one two three four five" — all five words appear in the transcript.
3. **Scenario 3**: First transcription result appears within 500 ms of speech onset after
   the silence period.
4. All scenarios run without a Go panic or unhandled gRPC error.
5. `go test -race` clean.

---

## Measurements to Record

- Time from audio start to first interim result (TTFI — time to first interim)
- Time from sentence end to final result (finalization latency)
- Whether stream restart causes any word loss (yes/no, and if yes, how many words)
- gRPC error codes observed (if any)

---

## What Failure Looks Like

- `ResourceExhausted` or `OutOfRange` errors at stream close → catch these specific error
  codes and trigger restart. Do not treat them as fatal.
- Audio dropped at stream boundary → increase replay buffer from 500 ms to 1000 ms.
- Poor WER → verify audio format matches config (linear16, 16 kHz, mono). Google STT is
  sensitive to header vs headerless PCM.

---

## Setup Notes

```bash
# GCP credentials
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account.json

# Generate testdata (requires sox or ffmpeg)
# 30 seconds of speech — use any real recording or text-to-speech
ffmpeg -i input.wav -f s16le -ar 16000 -ac 1 testdata/speech.raw

# For long_speech.raw: concatenate speech.raw 12 times
python3 -c "
data = open('testdata/speech.raw', 'rb').read()
with open('testdata/long_speech.raw', 'wb') as f:
    for _ in range(12):
        f.write(data)
"
```

Go dependency:
```
google.golang.org/api
cloud.google.com/go/speech/apiv1
```
