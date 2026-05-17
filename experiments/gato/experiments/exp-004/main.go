package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"
)

const (
	mainModelPath = "testdata/silero_vad.onnx"
	sampleRate    = int64(16000)
	chunkSamples  = 512 // 32ms at 16kHz
)

func main() {
	if _, err := os.Stat(mainModelPath); os.IsNotExist(err) {
		fmt.Println("Model not found. Run 'go test ./...' first to download it.")
		os.Exit(1)
	}

	vad, err := NewSileroVAD(mainModelPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	defer vad.Close()

	fmt.Printf("=== EXP-004: Silero VAD via CGO/ONNX ===\n")
	fmt.Printf("ONNX Runtime: shared session (Strategy B)\n")
	fmt.Printf("Platform: %s/%s, GOMAXPROCS=%d\n\n", runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0))

	scenario1(vad)
	scenario2(vad)
	scenario3(vad)
}

// scenario1: single stream, 500 chunks (500 * 32ms = 16 seconds of audio).
func scenario1(vad *SileroVAD) {
	fmt.Println("--- Scenario 1: Single Stream (500 chunks) ---")

	audio := makeSine512(440, float64(sampleRate))
	state := StreamState{}
	latencies := make([]time.Duration, 0, 500)

	for i := 0; i < 500; i++ {
		t0 := time.Now()
		prob, newState, err := vad.Infer(audio, sampleRate, state)
		lat := time.Since(t0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chunk %d: %v\n", i, err)
			return
		}
		_ = prob
		state = newState
		latencies = append(latencies, lat)
	}

	p50, p99 := sortedPercentiles(latencies)
	fmt.Printf("  Chunks: 500 | p50=%.2fms | p99=%.2fms\n", ms(p50), ms(p99))
	fmt.Printf("  p99 < 5ms: %v\n\n", p99 < 5*time.Millisecond)
}

// scenario2: N concurrent goroutines, each processing 50 chunks.
func scenario2(vad *SileroVAD) {
	fmt.Println("--- Scenario 2: Concurrent Streams ---")
	levels := []int{1, 10, 50, 100}

	for _, n := range levels {
		latencies := make([]time.Duration, 0, n*50)
		var mu sync.Mutex
		var wg sync.WaitGroup

		start := time.Now()
		for g := 0; g < n; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				audio := makeSine512(440, float64(sampleRate))
				state := StreamState{}
				for i := 0; i < 50; i++ {
					t0 := time.Now()
					prob, newState, err := vad.Infer(audio, sampleRate, state)
					lat := time.Since(t0)
					if err != nil {
						fmt.Fprintf(os.Stderr, "concurrent infer: %v\n", err)
						return
					}
					_ = prob
					state = newState

					mu.Lock()
					latencies = append(latencies, lat)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)

		p50, p99 := sortedPercentiles(latencies)
		fmt.Printf("  N=%3d  wall=%v  p50=%.2fms  p99=%.2fms  p99<10ms:%v\n",
			n, elapsed.Round(time.Millisecond), ms(p50), ms(p99), p99 < 10*time.Millisecond)
	}
	fmt.Println()
}

// scenario3: LSTM state isolation — two streams processed interleaved,
// verify states do not bleed across streams.
func scenario3(vad *SileroVAD) {
	fmt.Println("--- Scenario 3: LSTM State Isolation ---")

	sineAudio := makeSine512(440, float64(sampleRate))
	silenceAudio := make([]float32, 512)

	stateA := StreamState{}
	stateB := StreamState{}

	// Interleave 10 steps for each stream
	for i := 0; i < 10; i++ {
		_, sa, err := vad.Infer(sineAudio, sampleRate, stateA)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stream A error: %v\n", err)
			return
		}
		stateA = sa

		_, sb, err := vad.Infer(silenceAudio, sampleRate, stateB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stream B error: %v\n", err)
			return
		}
		stateB = sb
	}

	// Check divergence
	diverged := false
	for i := 0; i < 2; i++ {
		for k := 0; k < 128; k++ {
			if stateA.State[i][0][k] != stateB.State[i][0][k] {
				diverged = true
			}
		}
	}

	fmt.Printf("  State isolation after 10 interleaved steps: %v\n", diverged)
	fmt.Printf("  stateA.State[0][0][0]=%.6f  stateB.State[0][0][0]=%.6f\n",
		stateA.State[0][0][0], stateB.State[0][0][0])
	fmt.Printf("  Isolation confirmed: %v\n\n", diverged)
}

// --- Helpers ---

func makeSine512(freq float64, sr float64) []float32 {
	audio := make([]float32, 512)
	for i := range audio {
		audio[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/sr))
	}
	return audio
}

func sortedPercentiles(d []time.Duration) (p50, p99 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)*50/100], sorted[len(sorted)*99/100]
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
