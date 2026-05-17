package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestMain initialises the ONNX Runtime once for the entire test binary.
func TestMain(m *testing.M) {
	if err := initORT(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: ONNX Runtime init: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Load level tests
// ---------------------------------------------------------------------------

// TestLoad_Short is a 30-second quick sanity check (always uses short duration).
func TestLoad_Short(t *testing.T) {
	runLoadTest(t, 5, 30*time.Second)
}

// TestLoad_L1 runs 1 session for 30s (short) or 10m (full).
func TestLoad_L1(t *testing.T) {
	dur := 10 * time.Minute
	if testing.Short() {
		dur = 30 * time.Second
	}
	runLoadTest(t, 1, dur)
}

// TestLoad_L2 runs 10 sessions.
func TestLoad_L2(t *testing.T) {
	dur := 10 * time.Minute
	if testing.Short() {
		dur = 30 * time.Second
	}
	runLoadTest(t, 10, dur)
}

// TestLoad_L3 runs 25 sessions.
func TestLoad_L3(t *testing.T) {
	dur := 10 * time.Minute
	if testing.Short() {
		dur = 30 * time.Second
	}
	runLoadTest(t, 25, dur)
}

// TestLoad_L4 runs 50 sessions.
func TestLoad_L4(t *testing.T) {
	dur := 10 * time.Minute
	if testing.Short() {
		dur = 30 * time.Second
	}
	runLoadTest(t, 50, dur)
}

// TestLoad_Stability runs 50 sessions for 5m (short) or 30m (full).
func TestLoad_Stability(t *testing.T) {
	dur := 30 * time.Minute
	if testing.Short() {
		dur = 5 * time.Minute
	}
	runLoadTest(t, 50, dur)
}

// TestLoad_FailureInjection runs 5 failure scenarios.
func TestLoad_FailureInjection(t *testing.T) {
	if err := initORT(); err != nil {
		t.Fatalf("ONNX Runtime init: %v", err)
	}

	vad, err := NewSileroVAD("testdata/silero_vad.onnx")
	if err != nil {
		t.Fatalf("Load VAD: %v", err)
	}
	defer vad.Close()

	tts, err := newMockTTS("testdata/tts_response.raw")
	if err != nil {
		t.Fatalf("Load TTS audio: %v", err)
	}

	// Use a short per-scenario duration to keep total test time under 60s.
	scenarioDur := 10 * time.Second
	if !testing.Short() {
		scenarioDur = 60 * time.Second
	}

	t.Run("STT_Timeout", func(t *testing.T) {
		m := &SessionMetrics{}
		sess, err := newSession(0, vad, tts, "testdata/user_speech.raw", m)
		if err != nil {
			t.Fatal(err)
		}
		// Simulate STT timeout: 5s delay (so fewer turns complete).
		sess.stt.SetDelay(5 * time.Second)
		sess.forcedTurnInterval = 6 * time.Second // space turns to allow STT to finish

		ctx, cancel := context.WithTimeout(context.Background(), scenarioDur)
		defer cancel()
		// Should not panic; sessions with delayed STT just have fewer turns.
		sess.Run(ctx)
		t.Logf("STT timeout test complete. Turn-arounds: %d", len(m.turnAroundMs))
	})

	t.Run("TTS_Slow", func(t *testing.T) {
		m := &SessionMetrics{}
		// Slow TTS: 0.5× rate = 20ms per chunk instead of 10ms.
		slowTTS, err := newMockTTS("testdata/tts_response.raw")
		if err != nil {
			t.Fatal(err)
		}
		slowTTS.SetPaceMs(20.0)

		sess, err := newSession(0, vad, slowTTS, "testdata/user_speech.raw", m)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), scenarioDur)
		defer cancel()
		sess.Run(ctx)
		t.Logf("Slow TTS test complete. Turn-arounds: %d", len(m.turnAroundMs))
	})

	t.Run("Rapid_Interrupt", func(t *testing.T) {
		m := &SessionMetrics{}
		sess, err := newSession(0, vad, tts, "testdata/user_speech.raw", m)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), scenarioDur)
		defer cancel()

		// Fire rapid interrupts every 200ms for the test duration.
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					sess.Interrupt()
				case <-ctx.Done():
					return
				}
			}
		}()

		sess.Run(ctx)
		t.Logf("Rapid interrupt test complete. Interrupts: %d", m.interrupted)
	})

	t.Run("Context_Cancel_MidTurn", func(t *testing.T) {
		m := &SessionMetrics{}
		sess, err := newSession(0, vad, tts, "testdata/user_speech.raw", m)
		if err != nil {
			t.Fatal(err)
		}

		// Cancel after 5 seconds (mid-turn expected).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sess.Run(ctx) // should return cleanly
		t.Logf("Context cancel test complete.")
	})

	t.Run("ONNX_Error_Injection", func(t *testing.T) {
		// Use a separate VAD session to avoid affecting other tests.
		errVAD, err := NewSileroVAD("testdata/silero_vad.onnx")
		if err != nil {
			t.Fatal(err)
		}
		defer errVAD.Close()

		m := &SessionMetrics{}
		sess, err := newSession(0, errVAD, tts, "testdata/user_speech.raw", m)
		if err != nil {
			t.Fatal(err)
		}

		// Inject ONNX errors — VAD returns errors but session must not panic.
		errVAD.InjectError(true)

		ctx, cancel := context.WithTimeout(context.Background(), scenarioDur)
		defer cancel()

		// Should not panic even when VAD returns errors.
		sess.Run(ctx)
		t.Logf("ONNX error injection test complete. VAD infer count: %d", len(m.vadInferMs))
	})
}

// ---------------------------------------------------------------------------
// Core runner
// ---------------------------------------------------------------------------

func runLoadTest(t *testing.T, nSessions int, dur time.Duration) {
	t.Helper()

	if err := initORT(); err != nil {
		t.Fatalf("ONNX Runtime init: %v", err)
	}

	vad, err := NewSileroVAD("testdata/silero_vad.onnx")
	if err != nil {
		t.Fatalf("Load VAD: %v", err)
	}
	defer vad.Close()

	tts, err := newMockTTS("testdata/tts_response.raw")
	if err != nil {
		t.Fatalf("Load TTS audio: %v", err)
	}

	collector := &Collector{}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	goroutinesStart := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < nSessions; i++ {
		m := &SessionMetrics{}
		collector.AddSession(m)

		sess, err := newSession(i, vad, tts, "testdata/user_speech.raw", m)
		if err != nil {
			t.Fatalf("Create session %d: %v", i, err)
		}

		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			s.Run(ctx)
		}(sess)
	}

	// Collect metrics periodically.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				snap := collector.Collect(nSessions)
				t.Logf("[%s] sessions=%d goroutines=%d heap=%.1fMB ta_p50=%.1fms ta_p99=%.1fms vad_p99=%.2fms interrupts=%d",
					snap.Timestamp.Format("15:04:05"),
					snap.Sessions,
					snap.Goroutines,
					snap.HeapAllocMB,
					snap.TurnAroundP50Ms,
					snap.TurnAroundP99Ms,
					snap.VADLatencyP99Ms,
					snap.InterruptCount,
				)
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()

	// Final metrics.
	finalSnap := collector.Collect(nSessions)
	goroutinesEnd := runtime.NumGoroutine()

	t.Logf("=== Final Metrics (sessions=%d, duration=%v) ===", nSessions, dur)
	t.Logf("Turn-around: p50=%.1fms p99=%.1fms", finalSnap.TurnAroundP50Ms, finalSnap.TurnAroundP99Ms)
	t.Logf("VAD latency: p99=%.2fms", finalSnap.VADLatencyP99Ms)
	t.Logf("Goroutines: start=%d end=%d delta=%d (%.1f%%)",
		goroutinesStart, goroutinesEnd,
		goroutinesEnd-goroutinesStart,
		goroutineDeltaPct(goroutinesStart, goroutinesEnd),
	)
	t.Logf("Heap: %.1f MB", finalSnap.HeapAllocMB)
	t.Logf("GC pause p99: %.2fms", finalSnap.GCPauseP99Ms)
	t.Logf("Injected interrupts: %d", finalSnap.InterruptCount)

	// Pass criteria.
	p99Target := 300.0
	if nSessions >= 50 {
		p99Target = 500.0
	}

	if finalSnap.TurnAroundP99Ms > p99Target && finalSnap.TurnAroundP99Ms > 0 {
		t.Errorf("Turn-around p99 %.1fms exceeds target %.0fms for %d sessions",
			finalSnap.TurnAroundP99Ms, p99Target, nSessions)
	}

	goroutinePct := goroutineDeltaPct(goroutinesStart, goroutinesEnd)
	if goroutinePct > 5.0 {
		t.Errorf("Goroutine growth %.1f%% exceeds 5%% limit (start=%d end=%d)",
			goroutinePct, goroutinesStart, goroutinesEnd)
	}
}

func goroutineDeltaPct(start, end int) float64 {
	if start == 0 {
		return 0
	}
	delta := end - start
	if delta < 0 {
		delta = -delta
	}
	return float64(delta) / float64(start) * 100.0
}
