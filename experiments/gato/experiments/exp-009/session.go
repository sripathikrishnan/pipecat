package main

import (
	"context"
	"math/rand"
	"os"
	"sync"
	"time"
)

// SessionMetrics records per-turn latency and VAD inference times for one session.
type SessionMetrics struct {
	mu           sync.Mutex
	turnAroundMs []float64 // VAD-end → first audio byte (ms)
	vadInferMs   []float64 // each VAD inference duration (ms)
	interrupted  int       // count of injected interrupts
}

func (m *SessionMetrics) recordTurnAround(ms float64) {
	m.mu.Lock()
	m.turnAroundMs = append(m.turnAroundMs, ms)
	m.mu.Unlock()
}

func (m *SessionMetrics) recordVADInfer(ms float64) {
	m.mu.Lock()
	m.vadInferMs = append(m.vadInferMs, ms)
	m.mu.Unlock()
}

func (m *SessionMetrics) recordInterrupt() {
	m.mu.Lock()
	m.interrupted++
	m.mu.Unlock()
}

func (m *SessionMetrics) snapshot() (turnAround []float64, vadInfer []float64, interrupted int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ta := make([]float64, len(m.turnAroundMs))
	copy(ta, m.turnAroundMs)
	vi := make([]float64, len(m.vadInferMs))
	copy(vi, m.vadInferMs)
	return ta, vi, m.interrupted
}

// Session simulates one concurrent voice session. It contains:
//   - Input goroutine: feeds 20ms chunks at real-time rate
//   - VAD+turn goroutine: detects speech turns, calls STT, then TTS
//   - Output goroutine: drains AudioQueue at real-time rate (discards audio)
//
// Each session uses exactly 3 goroutines (plus sub-goroutines during TTS play).
type Session struct {
	id         int
	vad        *SileroVAD
	vadState   StreamState
	stt        *MockSTT
	tts        *MockTTS
	audioQueue *AudioQueue
	metrics    *SessionMetrics

	// inputAudio holds the pre-loaded user speech data.
	inputAudio []byte

	// interruptCh is used by the interrupt injector to cancel current TTS output.
	interruptCh chan struct{}

	// forcedTurnInterval, if > 0, fires a synthetic turn end at this interval
	// regardless of VAD output. Used when test audio (pure sine) is not
	// classified as speech by Silero VAD, to exercise the STT→TTS pipeline.
	forcedTurnInterval time.Duration
}

// newSession creates a new Session. The VAD is shared; each session gets its own
// STT and TTS instances and an independent AudioQueue.
// forcedTurnInterval, if > 0, causes the session to fire synthetic turns at that
// interval regardless of VAD output (useful when test audio is synthetic sine
// that Silero doesn't classify as speech).
func newSession(id int, vad *SileroVAD, tts *MockTTS, inputAudioPath string, metrics *SessionMetrics) (*Session, error) {
	data, err := os.ReadFile(inputAudioPath)
	if err != nil {
		return nil, err
	}
	return &Session{
		id:                 id,
		vad:                vad,
		stt:                newMockSTT(),
		tts:                tts,
		audioQueue:         newAudioQueue(512),
		metrics:            metrics,
		inputAudio:         data,
		interruptCh:        make(chan struct{}, 1),
		forcedTurnInterval: 3 * time.Second, // default: fire a turn every 3s
	}, nil
}

// Interrupt signals the session to abort its current TTS output early.
func (s *Session) Interrupt() {
	select {
	case s.interruptCh <- struct{}{}:
	default:
	}
}

// Run starts the session and blocks until ctx is cancelled.
// It launches 3 goroutines: input feeder, VAD+turn processor, and output drain.
func (s *Session) Run(ctx context.Context) {
	// Channel from input feeder → VAD+turn processor (buffered to absorb one chunk).
	inputCh := make(chan []byte, 4)

	var wg sync.WaitGroup
	wg.Add(3)

	// Goroutine 1: Input feeder — sends 20ms chunks at real-time rate.
	go func() {
		defer wg.Done()
		defer close(inputCh)
		s.runInputFeeder(ctx, inputCh)
	}()

	// Goroutine 2: VAD + turn processor.
	go func() {
		defer wg.Done()
		s.runVADAndTurn(ctx, inputCh)
	}()

	// Goroutine 3: Output drain — reads from AudioQueue and discards audio.
	go func() {
		defer wg.Done()
		s.runOutputDrain(ctx)
	}()

	wg.Wait()
}

// runInputFeeder loops over inputAudio in 320-byte (20ms at 16kHz mono int16) chunks,
// pacing delivery to real time using monotonic clock targeting.
func (s *Session) runInputFeeder(ctx context.Context, out chan<- []byte) {
	const chunkBytes = 320 // 160 samples × 2 bytes = 20ms at 16kHz mono int16
	const chunkDur = 20 * time.Millisecond

	n := len(s.inputAudio)
	if n == 0 {
		return
	}

	target := time.Now()
	offset := 0

	for {
		end := offset + chunkBytes
		if end > n {
			end = n
		}
		chunk := make([]byte, end-offset)
		copy(chunk, s.inputAudio[offset:end])

		select {
		case out <- chunk:
		case <-ctx.Done():
			return
		}

		offset = end
		if offset >= n {
			offset = 0 // loop back to simulate continuous speech
		}

		target = target.Add(chunkDur)
		delay := time.Until(target)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
		}
	}
}

// outputController manages the lifecycle of the current TTS output context.
// It is safe to use from a single goroutine (the VAD+turn goroutine),
// and the current TTS-play goroutine (which reads the context but never writes
// the cancelFunc). The only writer of cancelFunc is the VAD+turn goroutine.
type outputController struct {
	mu         sync.Mutex
	cancelFunc context.CancelFunc
}

// interrupt cancels the current TTS output and resets to a new idle context
// derived from parent. Safe to call concurrently.
func (oc *outputController) interrupt(parent context.Context) context.Context {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.cancelFunc != nil {
		oc.cancelFunc()
	}
	ctx, cancel := context.WithCancel(parent)
	oc.cancelFunc = cancel
	return ctx
}

// runVADAndTurn accumulates 20ms chunks into 512-sample (32ms) VAD windows,
// detects turn start/end, and drives the STT→TTS pipeline per turn.
//
// If s.forcedTurnInterval > 0, a synthetic turn-end fires at that interval
// regardless of VAD output. This allows the STT→TTS pipeline to be benchmarked
// even when test audio (pure sine wave) is not classified as speech by Silero VAD.
// The VAD continues running on every audio chunk for latency measurement purposes.
func (s *Session) runVADAndTurn(ctx context.Context, in <-chan []byte) {
	const (
		sampleRate      = 16000
		vadSamples      = 512          // 32ms at 16kHz
		speechThresh    = float32(0.5) // VAD probability threshold for speech
		turnStartFrames = 3            // 3 × 32ms = 96ms of speech → turn start
		turnEndFrames   = 16           // 16 × 32ms = 512ms of silence → turn end
	)

	// Accumulator: collects int16 samples before calling VAD.
	var accumSamples []int16

	// Convert accumulator to float32 for VAD.
	vadBuf := make([]float32, vadSamples)

	state := s.vadState
	var speechFrames, silenceFrames int
	inTurn := false

	// Interrupt injector: every 60–90s, fire an interrupt.
	interruptInterval := func() time.Duration {
		return time.Duration(60+rand.Intn(30)) * time.Second
	}
	interruptTimer := time.NewTimer(interruptInterval())
	defer interruptTimer.Stop()

	// Forced turn timer: fires a synthetic turn-end at a regular interval.
	var forcedTurnCh <-chan time.Time
	if s.forcedTurnInterval > 0 {
		ft := time.NewTicker(s.forcedTurnInterval)
		defer ft.Stop()
		forcedTurnCh = ft.C
	}

	// outCtl manages the current TTS output context.
	// Interrupt replaces it; the spawned TTS goroutine holds a snapshot
	// of the context at launch time — it never modifies outCtl.
	outCtl := &outputController{}
	outCtl.interrupt(ctx) // initialise with a live context
	defer func() { outCtl.interrupt(context.Background()) }()

	// interruptOutput cancels the current TTS play and resets the queue.
	interruptOutput := func() {
		outCtl.interrupt(ctx)
		s.audioQueue.Reset()
	}

	// fireTurn starts the STT→TTS pipeline for one turn end.
	// The spawned goroutine uses its own snapshot of the output context.
	fireTurn := func() {
		turnEndTime := time.Now()
		// Acquire a fresh output context for this turn.
		turnCtx := outCtl.interrupt(ctx)
		s.audioQueue.Reset()

		go func(tEnd time.Time, oc context.Context) {
			transcript, err := s.stt.Finalize(ctx)
			if err != nil {
				return // parent ctx cancelled
			}
			_ = transcript // in production, this feeds the LLM

			// Check if we were interrupted before TTS started.
			select {
			case <-oc.Done():
				return
			default:
			}

			firstByteCh := make(chan time.Time, 1)
			go func() {
				_ = s.tts.Play(oc, s.audioQueue, func() {
					firstByteCh <- time.Now()
				})
			}()

			// Wait for first audio byte to measure turnaround.
			select {
			case firstByteTime := <-firstByteCh:
				latencyMs := firstByteTime.Sub(tEnd).Seconds() * 1000.0
				s.metrics.recordTurnAround(latencyMs)
			case <-oc.Done():
			case <-ctx.Done():
			}
		}(turnEndTime, turnCtx)
	}

	processChunk := func(pcm []byte) {
		// Convert int16 bytes to samples.
		nSamples := len(pcm) / 2
		for i := 0; i < nSamples; i++ {
			s16 := int16(pcm[i*2]) | int16(pcm[i*2+1])<<8
			accumSamples = append(accumSamples, s16)
		}

		// Process all complete 512-sample windows.
		for len(accumSamples) >= vadSamples {
			window := accumSamples[:vadSamples]
			for i, v := range window {
				vadBuf[i] = float32(v) / 32768.0
			}
			accumSamples = accumSamples[vadSamples:]

			t0 := time.Now()
			prob, newState, err := s.vad.Infer(vadBuf, sampleRate, state)
			vadElapsed := time.Since(t0).Seconds() * 1000.0
			if err == nil {
				state = newState
				s.metrics.recordVADInfer(vadElapsed)
			}
			// On VAD error, continue without updating state.

			// Only use VAD for natural turn detection when forcedTurnInterval is 0.
			if s.forcedTurnInterval == 0 {
				if prob >= speechThresh {
					speechFrames++
					silenceFrames = 0
					if !inTurn && speechFrames >= turnStartFrames {
						inTurn = true
						s.audioQueue.Reset()
					}
				} else {
					silenceFrames++
					speechFrames = 0
					if inTurn && silenceFrames >= turnEndFrames {
						inTurn = false
						fireTurn()
						speechFrames = 0
						silenceFrames = 0
					}
				}
			}

			if inTurn {
				s.stt.RecordAudio(pcm)
			}
		}
	}

	for {
		select {
		case pcm, ok := <-in:
			if !ok {
				return
			}
			processChunk(pcm)

		case <-forcedTurnCh:
			// Forced turn: simulate a turn-end regardless of VAD.
			fireTurn()

		case <-interruptTimer.C:
			// Inject an interrupt: cancel current output.
			interruptOutput()
			s.metrics.recordInterrupt()
			interruptTimer.Reset(interruptInterval())

		case <-s.interruptCh:
			// External interrupt (from test harness).
			interruptOutput()
			s.metrics.recordInterrupt()

		case <-ctx.Done():
			return
		}
	}
}

// runOutputDrain consumes chunks from the AudioQueue and discards them,
// paced to real time (10ms per chunk at 48kHz).
func (s *Session) runOutputDrain(ctx context.Context) {
	const chunkDur = 10 * time.Millisecond

	target := time.Now()

	for {
		chunk, err := s.audioQueue.Get(ctx)
		if err != nil {
			return // ctx cancelled
		}
		_ = chunk // discard audio in this mock

		target = target.Add(chunkDur)
		delay := time.Until(target)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
		}
	}
}
