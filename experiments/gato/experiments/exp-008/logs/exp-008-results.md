# EXP-008 Results — Hello World Session

**Date**: 2026-05-17
**Status**: AUTOMATED TESTS PASS — manual browser test pending

---

## Automated Test Results

All 5 tests pass under `-race` (Go race detector):

```
=== RUN   TestTTS_Synthesize
    pipeline_test.go:60: synthesized 56896 bytes (1185.3 ms at 24kHz mono s16le)
--- PASS: TestTTS_Synthesize (0.74s)

=== RUN   TestSTT_Connect
    pipeline_test.go:86: STT connect + send OK
--- PASS: TestSTT_Connect (0.91s)

=== RUN   TestVAD_Load
    pipeline_test.go:113: VAD inference OK: prob=0.0006 (silence expected near 0)
--- PASS: TestVAD_Load (0.03s)

=== RUN   TestResampler_24to48
    pipeline_test.go:129: Resample: 480 bytes → 960 bytes (2× ratio OK)
--- PASS: TestResampler_24to48 (0.00s)

=== RUN   TestSession_NoGoroutineLeak
    pipeline_test.go:168: goroutines: baseline=4, after Run=6
    pipeline_test.go:183: goroutines after Close: 4 (baseline was 4)
--- PASS: TestSession_NoGoroutineLeak (0.18s)

PASS
ok  gato/exp008  3.454s
```

### Test Notes

- `TestTTS_Synthesize`: Real GCP call, synthesizes 1185ms of speech at 24kHz. ADC available.
- `TestSTT_Connect`: Real GCP call, streams 1s of silence, clean close. ADC available.
- `TestVAD_Load`: VAD model loads, inference on silence gives prob=0.0006 (correctly near zero).
- `TestResampler_24to48`: 480 bytes (10ms at 24kHz) → 960 bytes (20ms at 48kHz). 2× ratio correct.
- `TestSession_NoGoroutineLeak`: Session goroutines (2 extra: wakeOnCancel + audioTask) all cleaned up on cancel. Baseline=4, after Close=4.

---

## Build Instructions

```bash
cd experiments/gato/experiments/exp-008

# Prerequisites (one-time):
brew install opusfile pkg-config  # opusfile needed by github.com/hraban/opus

# Symlink VAD model (if not already present):
mkdir -p testdata
ln -sf ../../exp-004/testdata/silero_vad.onnx testdata/silero_vad.onnx

# Build:
CGO_LDFLAGS="-L/opt/homebrew/lib" \
CGO_CFLAGS="-I/opt/homebrew/include/onnxruntime" \
PKG_CONFIG_PATH="/opt/homebrew/lib/pkgconfig" \
go build ./...

# Run tests:
CGO_LDFLAGS="-L/opt/homebrew/lib" \
CGO_CFLAGS="-I/opt/homebrew/include/onnxruntime" \
PKG_CONFIG_PATH="/opt/homebrew/lib/pkgconfig" \
go test -v -race ./...

# Run the server:
CGO_LDFLAGS="-L/opt/homebrew/lib" \
CGO_CFLAGS="-I/opt/homebrew/include/onnxruntime" \
PKG_CONFIG_PATH="/opt/homebrew/lib/pkgconfig" \
go run .
# Then open http://localhost:8080 in Chrome or Firefox
```

**Note**: The build produces a harmless warning:
`ld: warning: ignoring duplicate libraries: '-lopus'`
This is because both cgo.go and pkg-config add `-lopus`. Safe to ignore.

---

## Manual Browser Test Checklist

Test scenario: open http://localhost:8080, click Connect, speak into mic.

| Scenario | Expected | Actual | Pass? |
|---|---|---|---|
| Page loads with Connect button | HTML renders cleanly | | |
| Click Connect → mic permission | Browser asks for mic access | | |
| WebRTC negotiation | Status shows "connected" | | |
| Speak "hello" → VAD indicator turns green | Green dot appears within ~60ms | | |
| Stop speaking → VAD indicator turns grey | Grey dot within ~500ms | | |
| STT log entry appears | STT: "hello" shown in log | | |
| Bot replies via speaker | Hear "You said: hello" | | |
| Bot text shown in log | BOT: "You said: hello" in log | | |
| Interrupt: speak mid-bot-reply | Bot stops, new turn starts | | |
| INTERRUPT log entry with heard text | Shows what bot had said | | |
| Disconnect button | Mic released, status "Disconnected" | | |
| Multiple turns in sequence | Each turn produces STT + bot response | | |

---

## Findings from Automated Tests

1. **Full pipeline compiles and links cleanly.** All 7 prior experiments wire together without import conflicts or CGO symbol collisions. onnxruntime and opus coexist.

2. **opusfile required by hraban/opus.** The `github.com/hraban/opus` package needs `opusfile` for its stream reader (`stream.go`). This is not needed at runtime for RTP decode — only the Opus decoder/encoder code is used. However, because stream.go is in the same package, opusfile must be present at compile time. Install with `brew install opusfile`.

3. **Goroutine leak test confirms clean lifecycle.** Session launches 2 goroutines (audioTask + wakeOnCancel). Both exit within 100ms of Close(). No leak.

4. **Monotonic clock pacing implemented.** EXP-007 identified ~10% cumulative drift from `time.Sleep`. EXP-008 implements monotonic clock targeting (`target += 10ms; sleep = time.Until(target)`), which is self-correcting.

5. **STT uses session context directly.** Rather than a separate per-turn context with cancel function (which triggered go vet lostcancel false positive), STT.Close() is called directly to stop the stream. The STT goroutine exits when Close() is called because the `done` channel is closed.

6. **go vet lostcancel false positive.** Initial design used `sttCtx, sttCancel = context.WithCancel(ctx)` inside a loop. go vet flagged this as a potential context leak even with a defer cleanup. Resolved by removing the per-turn context and using STT.Close() directly. A no-op initializer pattern (`var sttCancel context.CancelFunc = func() {}`) is a documented workaround for this vet rule.
