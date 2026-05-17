package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

const (
	modelPath = "testdata/silero_vad.onnx"
	modelURL  = "https://raw.githubusercontent.com/snakers4/silero-vad/master/src/silero_vad/data/silero_vad.onnx"
)

// TestMain downloads the Silero VAD model if not present, then runs all tests.
func TestMain(m *testing.M) {
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		if err := downloadModel(); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: failed to download model: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func downloadModel() error {
	fmt.Printf("Downloading Silero VAD model from %s ...\n", modelURL)
	resp, err := http.Get(modelURL)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	if err := os.MkdirAll("testdata", 0o755); err != nil {
		return fmt.Errorf("mkdir testdata: %w", err)
	}

	f, err := os.Create(modelPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	fmt.Printf("Downloaded %d bytes to %s\n", n, modelPath)
	return nil
}

// sharedVAD is a package-level singleton to avoid reloading per test.
var (
	sharedVAD     *SileroVAD
	sharedVADOnce sync.Once
	sharedVADErr  error
)

func getVAD(t *testing.T) *SileroVAD {
	t.Helper()
	sharedVADOnce.Do(func() {
		sharedVAD, sharedVADErr = NewSileroVAD(modelPath)
	})
	if sharedVADErr != nil {
		t.Fatalf("NewSileroVAD: %v", sharedVADErr)
	}
	return sharedVAD
}

// sine512 generates a 512-sample sine wave at the given frequency.
func sine512(freq float64, sampleRate float64) []float32 {
	audio := make([]float32, 512)
	for i := range audio {
		audio[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/sampleRate))
	}
	return audio
}

// zeros512 generates 512 zero (silence) samples.
func zeros512() []float32 {
	return make([]float32, 512)
}

// --- Tests ---

// TestVAD_Load verifies the model loads without error.
func TestVAD_Load(t *testing.T) {
	vad, err := NewSileroVAD(modelPath)
	if err != nil {
		t.Fatalf("NewSileroVAD failed: %v", err)
	}
	defer vad.Close()
	t.Log("Model loaded successfully")
}

// TestVAD_SpeechDetection runs inference on a 440Hz sine wave (simulated speech-like signal).
// We only check that inference runs without error and returns a probability in [0, 1].
// A sine wave is not real speech, so we do not check for prob > threshold.
func TestVAD_SpeechDetection(t *testing.T) {
	vad := getVAD(t)

	audio := sine512(440, 16000)
	state := StreamState{}

	prob, newState, err := vad.Infer(audio, 16000, state)
	if err != nil {
		t.Fatalf("Infer error: %v", err)
	}
	if prob < 0 || prob > 1 {
		t.Errorf("prob out of range [0,1]: %f", prob)
	}
	t.Logf("440Hz sine wave: prob=%.4f (state[0][0][0]=%.6f)", prob, newState.State[0][0][0])
}

// TestVAD_SilenceDetection runs inference on a zero-sample (silence) buffer.
func TestVAD_SilenceDetection(t *testing.T) {
	vad := getVAD(t)

	audio := zeros512()
	state := StreamState{}

	prob, _, err := vad.Infer(audio, 16000, state)
	if err != nil {
		t.Fatalf("Infer error on silence: %v", err)
	}
	if prob < 0 || prob > 1 {
		t.Errorf("prob out of range [0,1]: %f", prob)
	}
	t.Logf("Silence: prob=%.4f", prob)
}

// TestVAD_StateIsolation verifies that two StreamState instances are fully independent.
// Strategy B passes state as input tensors, so isolation is structural — no session
// mutation can bleed between callers. This test confirms it empirically.
func TestVAD_StateIsolation(t *testing.T) {
	vad := getVAD(t)

	audio := sine512(440, 16000)
	stateA := StreamState{}
	stateB := StreamState{}

	// Run 5 inference steps on stream A with sine
	for i := 0; i < 5; i++ {
		_, s, err := vad.Infer(audio, 16000, stateA)
		if err != nil {
			t.Fatalf("stream A infer %d: %v", i, err)
		}
		stateA = s
	}

	// Run 5 inference steps on stream B with silence
	silence := zeros512()
	for i := 0; i < 5; i++ {
		_, s, err := vad.Infer(silence, 16000, stateB)
		if err != nil {
			t.Fatalf("stream B infer %d: %v", i, err)
		}
		stateB = s
	}

	// States must diverge (sine vs silence drives different LSTM evolution)
	diverged := false
	for i := 0; i < 2; i++ {
		for k := 0; k < 128; k++ {
			if stateA.State[i][0][k] != stateB.State[i][0][k] {
				diverged = true
				break
			}
		}
	}

	if !diverged {
		t.Error("streams A and B have identical hidden state after different inputs — isolation failure")
	} else {
		t.Logf("State isolation confirmed: stateA[0][0][0]=%.6f, stateB[0][0][0]=%.6f",
			stateA.State[0][0][0], stateB.State[0][0][0])
	}
}

// TestVAD_Concurrent runs 50 goroutines, each calling Infer 25 times concurrently.
// Measures p50/p99 latency. Must complete within 60 seconds.
func TestVAD_Concurrent(t *testing.T) {
	const (
		numGoroutines     = 50
		inferPerGoroutine = 25
	)

	vad := getVAD(t)
	audio := sine512(440, 16000)

	latencies := make([]time.Duration, 0, numGoroutines*inferPerGoroutine)
	var mu sync.Mutex
	var wg sync.WaitGroup

	start := time.Now()

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			state := StreamState{}
			for i := 0; i < inferPerGoroutine; i++ {
				t0 := time.Now()
				prob, newState, err := vad.Infer(audio, 16000, state)
				elapsed := time.Since(t0)
				if err != nil {
					// Can't call t.Fatal from goroutine; log and return
					t.Errorf("goroutine %d infer %d: %v", goroutineID, i, err)
					return
				}
				_ = prob
				state = newState

				mu.Lock()
				latencies = append(latencies, elapsed)
				mu.Unlock()
			}
		}(g)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent test timed out after 60s")
	}

	elapsed := time.Since(start)
	t.Logf("Total wall time: %v for %d goroutines × %d infers = %d calls",
		elapsed, numGoroutines, inferPerGoroutine, numGoroutines*inferPerGoroutine)

	p50, p99 := percentiles(latencies)
	t.Logf("Latency p50=%.2fms  p99=%.2fms", p50.Seconds()*1000, p99.Seconds()*1000)

	if p99 > 10*time.Millisecond {
		t.Errorf("p99 latency %.2fms exceeds 10ms target", p99.Seconds()*1000)
	}
}

// BenchmarkVAD_SingleStream measures single-stream throughput.
func BenchmarkVAD_SingleStream(b *testing.B) {
	vad, err := NewSileroVAD(modelPath)
	if err != nil {
		b.Fatalf("NewSileroVAD: %v", err)
	}
	defer vad.Close()

	audio := sine512(440, 16000)
	state := StreamState{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prob, newState, err := vad.Infer(audio, 16000, state)
		if err != nil {
			b.Fatalf("Infer: %v", err)
		}
		_ = prob
		state = newState
	}
}

// percentiles computes p50 and p99 from a slice of durations (modifies slice order).
func percentiles(d []time.Duration) (p50, p99 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	// Simple insertion sort — list is small enough
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
	p50 = d[len(d)*50/100]
	p99 = d[len(d)*99/100]
	return
}
