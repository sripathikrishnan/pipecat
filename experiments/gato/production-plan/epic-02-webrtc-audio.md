# Epic 02: WebRTC Audio I/O

## Business Meaning

Voice travels into Gato from the browser as Opus-encoded WebRTC audio, and Gato's synthesized speech travels back the same way. This epic makes that two-way audio channel work correctly: ICE negotiation completes, audio is encoded and decoded with the right parameters, and the connection recovers gracefully if the network hiccups.

Without this epic, Gato is deaf and mute. With it, the browser can hear Gato and Gato can hear the browser.

---

## Background

The WebRTC layer is implemented with **Pion** (`github.com/pion/webrtc/v4`). All architectural questions were resolved in the experiments:

- **EXP-001**: Push model (server calls `WriteSample`) with monotonic clock targeting; 2ms interrupt latency
- **EXP-008**: Full input/output path: `TrackRemote` Opus decode → 48kHz PCM; 48kHz PCM → Opus encode → `TrackLocalStaticSample.WriteSample`
- **EXP-010 (critical bug)**: `TrackLocalStaticSample` does NOT auto-encode PCM. `WriteSample` passes bytes directly as the RTP payload. Must encode Opus before calling it.
- **EXP-010 (critical bug)**: Last TTS chunk must be zero-padded to exactly 960 bytes (480 int16 samples at 48kHz, 10ms). The Opus encoder rejects any other frame size.

**Existing reference implementation**: `experiments/gato/experiments/exp-008/pipeline.go` — `handleInputTrack()` and `audioTaskRun()`.

---

## Tasks

### Task 2.1 — Pion PeerConnection factory

Create `gato/internal/webrtc/connection.go`.

`NewPeerConnection(config webrtc.Configuration) (*webrtc.PeerConnection, error)` builds a `PeerConnection` with:
- Codec: `webrtc.MimeTypeOpus`, 48kHz, mono (channels=2 in SDP but decoded as mono for VAD)
- DTLS role: passive (Gato is the server)
- ICE transport policy: all (relay + host + srflx)

`AddOutputTrack(pc *webrtc.PeerConnection) (*webrtc.TrackLocalStaticSample, error)` creates the outbound audio track and adds it to the PeerConnection. The track ID and stream ID must be stable per session (not random) so ICE restart doesn't confuse the browser. Use `session_id` as the stream ID.

### Task 2.2 — SDP offer/answer exchange via signal channel

Create `gato/internal/webrtc/negotiate.go`.

`Negotiate(ctx context.Context, pc *webrtc.PeerConnection, signals <-chan []byte, send func([]byte) error) error`:

1. Wait for the first signal message on `signals` (the browser's SDP offer)
2. `pc.SetRemoteDescription(offer)`
3. `pc.CreateAnswer(nil)` → `pc.SetLocalDescription(answer)`
4. Marshal answer as JSON; call `send(answerJSON)`
5. Continue reading from `signals` for trickle ICE candidates: each `{"candidate": "..."}` message → `pc.AddICECandidate()`
6. Block until `pc.ICEConnectionState() == ICEConnectionStateConnected` or `ctx.Done()`

The `signals` channel is the one populated by `handleBrowserSignal` in Epic 01. The `send` function writes an `AgentToBrowserSignal` via the Switchboard client's send queue.

Timeout: 30s total for ICE to reach Connected state (configurable via `GATO_T_WEBRTC_MS`).

### Task 2.3 — Input track: Opus decode and 48→16kHz decimation

This code lives in `gato/internal/session/session.go`, method `handleInputTrack`.

The existing reference is `experiments/gato/experiments/exp-008/pipeline.go:145-293`.

Key details:
- `opus.NewDecoder(48000, 1)` — 48kHz mono
- `track.ReadRTP()` delivers Opus packets; each decodes to 960 int16 samples (20ms at 48kHz)
- Decimate 3:1 to get 320 samples at 16kHz (take every 3rd sample)
- Accumulate into `vadBuf []float32`; process full 512-sample VAD chunks
- Maintain 64-sample context buffer (`vadContext [ContextSize]float32`) — prepend to each chunk before calling `vad.Infer()`
- Pass decoded 16kHz PCM bytes to STT while in turn

Package the 48→16kHz decimation in `gato/internal/audio/decimate.go` as a testable function `Decimate3(in []int16) []int16`. Do not inline it.

### Task 2.4 — Opus encoder for output

Create `gato/internal/codec/opus_encoder.go`.

```go
type OpusEncoder struct {
    enc *opus.Encoder
}

func NewOpusEncoder() (*OpusEncoder, error)   // 48kHz, mono, AppVoIP
func (e *OpusEncoder) Encode(pcm []int16) ([]byte, error)
func (e *OpusEncoder) Reset()                 // reset internal state on session restart
```

The encoder is per-session (not shared). It must be created in `NewSession` and referenced only from `audioTaskRun` (single goroutine — no mutex needed).

`Encode` must receive exactly 480 int16 samples (960 bytes / 10ms). If the caller passes fewer, it must panic (programming error) in development and be caught as a critical metric in production.

### Task 2.5 — Output track: Opus encode + WriteSample

`audioTaskRun` in `gato/internal/session/session.go`.

The existing reference is `experiments/gato/experiments/exp-008/pipeline.go:356-413`.

Key points:
- Monotonic clock targeting: `target = target.Add(10ms)` then `time.Sleep(time.Until(target))` — never drift by sleeping a flat 10ms each iteration
- Convert PCM bytes → `[]int16` → `encoder.Encode()` → `outputTrack.WriteSample(media.Sample{Data: opusBuf[:n], Duration: 10ms})`
- On `StopAudioFrame`: reset `target = time.Now()` (bot utterance complete; don't continue counting against a stale clock)
- On `EndAudioFrame`: return (session shutting down)
- On `OpusEncoder.Encode` error: log at error level and emit a metrics counter; do NOT crash the goroutine

### Task 2.6 — ICE restart (RESUMING state)

When Switchboard transitions a session to RESUMING (browser reconnected after network drop), Gato must:

1. Detect the `SessionRelease` with `reason: "ice_restart"` or a Switchboard-specific `ICERestart` signal (confirm with Switchboard source)
2. Call `pc.RestartICE()`
3. Re-run `Negotiate()` with the new signal channel
4. Resume `audioTaskRun` from where it left off (the `AudioQueue` is not drained; the audio continues)

The Pion `PeerConnection` supports ICE restart without recreating the connection, so the STT stream, VAD state, and Opus encoder state are preserved.

This is a stretch goal; implement only if the Switchboard's RESUMING state path is exercised in the E2E test.

---

## Definition of Done

- [ ] Browser (Chrome/Safari) can make a WebRTC call to Gato and hear the stub TTS response
- [ ] Gato can decode incoming Opus audio to 16kHz PCM (verified by piping to a WAV file and listening)
- [ ] Gato can encode 48kHz PCM to Opus and stream it as `WriteSample` — browser hears intelligible audio
- [ ] No `opus: invalid argument` errors in logs (last-chunk padding is correct)
- [ ] Interrupt: if new audio starts while output is playing, `audioTaskRun` stops within one chunk (10ms)
- [ ] Memory stable over a 5-minute call (no accumulating goroutines or buffers)

---

## Verification

### Unit Tests

- `TestDecimate3`: feed 960 48kHz samples; assert output is 320 samples; verify spectral content preserved (compare a 440Hz sine wave before/after)
- `TestOpusEncoderRoundTrip`: encode 480 zero samples; assert output is valid Opus packet (non-empty, no error)
- `TestOpusEncoderWrongSize`: call `Encode` with 479 samples; assert error or panic (never silently corrupt audio)
- `TestAudioTaskRun_MonotonicDrift`: feed 100 frames; record wall-clock time; assert total time is within 5% of 1000ms
- `TestAudioTaskRun_StopFrameResetsTarget`: inject `StopAudioFrame` then new audio; assert no cumulative drift from before the stop

### Integration Tests

- `TestNegotiate_OfferAnswer`: create two Pion PeerConnections in-process (no real network); run `Negotiate` on one; verify ICE Connected
- `TestNegotiate_Timeout`: don't send ICE candidates; assert context deadline exceeded within `T_WEBRTC_MS`
- `TestInputTrack_Decodes`: set up a fake Pion track; inject pre-encoded Opus packets from the EXP-010 reference WAV; assert decoded PCM is non-zero

### Browser + Server E2E

Use the `experiments/gato/experiments/exp-010/client/e2e_client.py` aiortc client (already proven working). Run with a short 10-second WAV clip:

```bash
python e2e_client.py --input tests/fixtures/hello.wav --output /tmp/output.wav --timeout 30
```

Assert:
- `ffprobe /tmp/output.wav` shows duration > 1 second (TTS responded)
- `ffprobe -f lavfi -i amix=... volumedetect` shows mean volume > -40 dBFS (not silence)
- Server logs show no `opus encode` errors
