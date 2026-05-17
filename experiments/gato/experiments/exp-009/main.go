package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

func main() {
	sessions := flag.Int("sessions", 10, "Number of concurrent sessions")
	duration := flag.Duration("duration", 10*time.Minute, "Test duration")
	pprofAddr := flag.String("pprof", "", "pprof HTTP address (e.g. :6060), empty = disabled")
	outputCSV := flag.String("output-csv", "", "Optional CSV output file path")
	flag.Parse()

	// Start pprof server if requested.
	if *pprofAddr != "" {
		go func() {
			log.Printf("pprof listening on %s", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				log.Printf("pprof server error: %v", err)
			}
		}()
	}

	if err := initORT(); err != nil {
		log.Fatalf("ONNX Runtime init: %v", err)
	}

	// Load shared VAD model.
	vad, err := NewSileroVAD("testdata/silero_vad.onnx")
	if err != nil {
		log.Fatalf("Load VAD: %v", err)
	}
	defer vad.Close()

	// Load mock TTS audio.
	tts, err := newMockTTS("testdata/tts_response.raw")
	if err != nil {
		log.Fatalf("Load TTS audio: %v", err)
	}

	// Setup context with timeout.
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	// Handle SIGINT/SIGTERM gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			log.Println("Signal received, shutting down...")
			cancel()
		case <-ctx.Done():
		}
	}()

	// Create sessions.
	collector := &Collector{}
	var wg sync.WaitGroup
	goroutinesStart := runtime.NumGoroutine()

	for i := 0; i < *sessions; i++ {
		m := &SessionMetrics{}
		collector.AddSession(m)

		sess, err := newSession(i, vad, tts, "testdata/user_speech.raw", m)
		if err != nil {
			log.Fatalf("Create session %d: %v", i, err)
		}

		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			s.Run(ctx)
		}(sess)
	}

	log.Printf("Started %d sessions, running for %v", *sessions, *duration)
	fmt.Println()

	// Periodic metrics collection.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				snap := collector.Collect(*sessions)
				fmt.Printf("[%s] sessions=%d goroutines=%d heap=%.1fMB ta_p50=%.1fms ta_p99=%.1fms vad_p99=%.2fms interrupts=%d\n",
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

	// Wait for all sessions to finish.
	wg.Wait()

	// Final collection.
	finalSnap := collector.Collect(*sessions)
	goroutinesEnd := runtime.NumGoroutine()

	fmt.Println()
	fmt.Println("=== EXP-009 Final Summary ===")
	collector.PrintTable(os.Stdout)
	fmt.Println()
	fmt.Printf("Goroutines: start=%d end=%d delta=%d\n", goroutinesStart, goroutinesEnd, goroutinesEnd-goroutinesStart)
	fmt.Printf("Turn-around p50=%.1fms p99=%.1fms\n", finalSnap.TurnAroundP50Ms, finalSnap.TurnAroundP99Ms)
	fmt.Printf("VAD latency p99=%.2fms\n", finalSnap.VADLatencyP99Ms)
	fmt.Printf("Injected interrupts: %d\n", finalSnap.InterruptCount)

	// Write CSV if requested.
	if *outputCSV != "" {
		f, err := os.Create(*outputCSV)
		if err != nil {
			log.Printf("Cannot create CSV file: %v", err)
		} else {
			defer f.Close()
			collector.PrintCSV(f)
			fmt.Printf("CSV written to %s\n", *outputCSV)
		}
	}
}
