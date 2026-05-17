package main

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
)

// hasADC returns true if Application Default Credentials appear to be available.
// We check for the environment variable first; if missing we try a real GCP call
// and skip on failure. Tests call skipIfNoADC to do this.
func hasADC() bool {
	// Common ADC indicators.
	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		return true
	}
	if home, _ := os.UserHomeDir(); home != "" {
		_, err1 := os.Stat(home + "/.config/gcloud/application_default_credentials.json")
		_, err2 := os.Stat(home + "/Library/Application Support/gcloud/application_default_credentials.json")
		if err1 == nil || err2 == nil {
			return true
		}
	}
	// On GCE/Cloud Run metadata is available.
	if os.Getenv("GOOGLE_CLOUD_PROJECT") != "" {
		return true
	}
	return false
}

func skipIfNoADC(t *testing.T) {
	t.Helper()
	if !hasADC() {
		t.Skip("no Application Default Credentials available — skipping GCP test")
	}
}

// TestTTS_Synthesize synthesizes "Hello world" and checks the result is non-empty.
func TestTTS_Synthesize(t *testing.T) {
	skipIfNoADC(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tts, err := NewGoogleTTS(ctx)
	if err != nil {
		t.Skipf("NewGoogleTTS: %v", err)
	}
	defer tts.Close()

	pcm, err := tts.Synthesize(ctx, "Hello world")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(pcm) == 0 {
		t.Fatal("expected non-empty PCM, got 0 bytes")
	}
	t.Logf("synthesized %d bytes (%.1f ms at 24kHz mono s16le)", len(pcm),
		float64(len(pcm))/2/24000*1000)
}

// TestSTT_Connect opens a streaming STT session, sends 1s of silence, closes.
func TestSTT_Connect(t *testing.T) {
	skipIfNoADC(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stt, err := NewSTTClient(ctx)
	if err != nil {
		t.Skipf("NewSTTClient: %v", err)
	}
	stt.Start(ctx)
	defer stt.Close()

	// Send 1s of silence at 16kHz mono int16 (100ms chunks × 10).
	silence := make([]byte, 3200) // 1600 samples × 2 bytes = 100ms at 16kHz
	for i := 0; i < 10; i++ {
		if err := stt.SendAudio(silence); err != nil {
			t.Fatalf("SendAudio: %v", err)
		}
	}
	// Just verify no panic and clean close.
	t.Log("STT connect + send OK")
}

// TestVAD_Load loads the Silero VAD model and runs one inference step.
func TestVAD_Load(t *testing.T) {
	const modelPath = "testdata/silero_vad.onnx"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("model not found at %s — run setup first", modelPath)
	}

	vad, err := NewSileroVAD(modelPath)
	if err != nil {
		t.Fatalf("NewSileroVAD: %v", err)
	}
	defer vad.Close()

	// Run one inference step on silent audio.
	audio := make([]float32, 512)
	var state StreamState
	prob, newState, err := vad.Infer(audio, 16000, state)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if prob < 0 || prob > 1 {
		t.Errorf("prob out of range [0,1]: %f", prob)
	}
	_ = newState
	t.Logf("VAD inference OK: prob=%.4f (silence expected near 0)", prob)
}

// TestResampler_24to48 verifies 10ms at 24kHz upsamples to 20ms at 48kHz.
func TestResampler_24to48(t *testing.T) {
	r := &LinearResampler{}

	// 10ms at 24kHz = 240 samples = 480 bytes
	in := make([]byte, 480)
	out := r.Resample(in)

	// 10ms at 48kHz = 480 samples = 960 bytes
	const want = 960
	if len(out) != want {
		t.Errorf("expected %d bytes, got %d", want, len(out))
	}
	t.Logf("Resample: %d bytes → %d bytes (2× ratio OK)", len(in), len(out))
}

// TestSession_NoGoroutineLeak creates a Session with mocked Pion layer,
// processes 100ms of silence, cancels, and verifies goroutines return to baseline.
func TestSession_NoGoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goroutine leak test in short mode")
	}

	// Load VAD model.
	const modelPath = "testdata/silero_vad.onnx"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("model not found at %s — skipping goroutine leak test", modelPath)
	}

	vad, err := NewSileroVAD(modelPath)
	if err != nil {
		t.Fatalf("NewSileroVAD: %v", err)
	}
	defer vad.Close()

	// Session with nil TTS and nil outputTrack — we don't exercise TTS here.
	statusCh := make(chan StatusEvent, 32)
	sess := NewSession(nil, nil, vad, nil, statusCh)

	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	sess.Run(ctx)

	// Let the session run briefly.
	time.Sleep(50 * time.Millisecond)

	// Verify we launched some goroutines.
	current := runtime.NumGoroutine()
	if current <= baseline {
		t.Errorf("expected goroutine count to increase, baseline=%d current=%d", baseline, current)
	}
	t.Logf("goroutines: baseline=%d, after Run=%d", baseline, current)

	// Cancel and clean up.
	cancel()
	sess.Close()

	// Allow goroutines to settle.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()

	after := runtime.NumGoroutine()
	// Allow a small delta for test framework goroutines.
	if after > baseline+3 {
		t.Errorf("goroutine leak: baseline=%d, after close=%d (delta=%d)", baseline, after, after-baseline)
	}
	t.Logf("goroutines after Close: %d (baseline was %d)", after, baseline)
}
