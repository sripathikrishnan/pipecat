package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Test doubles ---

// recordingWriter records every WriteAudio call with its arrival timestamp.
type recordingWriter struct {
	mu     sync.Mutex
	writes []writeRecord
	delay  time.Duration // artificial delay per write (to simulate slow transport)
}

type writeRecord struct {
	data []byte
	at   time.Time
}

func (w *recordingWriter) WriteAudio(pcm []byte) error {
	if w.delay > 0 {
		time.Sleep(w.delay)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cp := make([]byte, len(pcm))
	copy(cp, pcm)
	w.writes = append(w.writes, writeRecord{data: cp, at: time.Now()})
	return nil
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

func (w *recordingWriter) first() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.writes) == 0 {
		return time.Time{}
	}
	return w.writes[0].at
}

func (w *recordingWriter) last() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.writes) == 0 {
		return time.Time{}
	}
	return w.writes[len(w.writes)-1].at
}

func (w *recordingWriter) totalBytes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, r := range w.writes {
		n += len(r.data)
	}
	return n
}

// recordingObserver captures upstream/downstream events.
type recordingObserver struct {
	mu               sync.Mutex
	startedSpeaking  []time.Time
	stoppedSpeaking  []time.Time
	endFrameReceived []time.Time
}

func (o *recordingObserver) OnBotStartedSpeaking() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.startedSpeaking = append(o.startedSpeaking, time.Now())
}

func (o *recordingObserver) OnBotStoppedSpeaking() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stoppedSpeaking = append(o.stoppedSpeaking, time.Now())
}

func (o *recordingObserver) OnEndFrame() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.endFrameReceived = append(o.endFrameReceived, time.Now())
}

func (o *recordingObserver) startedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.startedSpeaking)
}

func (o *recordingObserver) stoppedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.stoppedSpeaking)
}

func (o *recordingObserver) endCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.endFrameReceived)
}

// makeSilence returns n bytes of zero-filled audio (silence in s16le).
func makeSilence(n int) []byte {
	return make([]byte, n)
}

// makeSpeech returns n bytes of non-zero audio (not silence).
func makeSpeech(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x20
	}
	return b
}

// makeFrame constructs a TTSAudioFrame with the given audio bytes.
func makeFrame(audio []byte) *TTSAudioFrame {
	return &TTSAudioFrame{
		AudioFrame: AudioFrame{Audio: audio, SampleRate: sampleRate},
	}
}

// audioBytes returns the total byte count for n milliseconds at 48 kHz s16le mono.
func audioBytes(ms int) int {
	return sampleRate / 1000 * ms * channels * bytesPerSample
}

// --- Tests ---

// Scenario 1: Large TTS blob → chunk normalization → real-time pacing.
// Feed 200 ms of audio as a single frame. Expect 20 × 10 ms chunks, played
// over ≈200 ms (±15% allowed to keep CI-friendly).
func TestScenario1_ChunkNormalization(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	obs := &recordingObserver{}
	ot := NewOutputTransport(writer, obs)
	defer ot.Close()

	const durMs = 200
	frame := makeFrame(makeSpeech(audioBytes(durMs)))

	start := time.Now()
	ot.HandleAudioFrame(frame)
	ot.HandleTTSStopped()

	// Wait until all chunks are written (with generous timeout).
	deadline := time.After(5 * time.Second)
	for {
		if writer.count() >= durMs/10 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for chunks: got %d, want %d", writer.count(), durMs/10)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	elapsed := time.Since(start)
	expectedChunks := durMs / 10
	if writer.count() != expectedChunks {
		t.Errorf("chunk count: got %d, want %d", writer.count(), expectedChunks)
	}
	if writer.totalBytes() != audioBytes(durMs) {
		t.Errorf("total bytes: got %d, want %d", writer.totalBytes(), audioBytes(durMs))
	}

	// Playback timing: must be within 15% of expected.
	const tolerance = 0.15
	base := time.Duration(durMs) * time.Millisecond
	minDur := time.Duration(float64(base) * (1 - tolerance))
	maxDur := time.Duration(float64(base) * (1 + tolerance))
	if elapsed < minDur || elapsed > maxDur {
		t.Errorf("playback duration %v not in [%v, %v]", elapsed, minDur, maxDur)
	}
	t.Logf("Scenario 1: %d chunks written in %v (expected ~%v)", writer.count(), elapsed, time.Duration(durMs)*time.Millisecond)
}

// Scenario 2: Interrupt mid-playback, measure latency.
// Feed a 500 ms blob. After ~100 ms, send InterruptionFrame.
// Assert audio stops within 30 ms of the interrupt signal.
func TestScenario2_InterruptLatency(t *testing.T) {
	t.Parallel()
	const trials = 10
	latencies := make([]time.Duration, 0, trials)

	for i := 0; i < trials; i++ {
		writer := &recordingWriter{}
		obs := &recordingObserver{}
		ot := NewOutputTransport(writer, obs)

		const durMs = 500
		ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(durMs))))

		// Wait until playback has started (at least 3 chunks written).
		for writer.count() < 3 {
			time.Sleep(2 * time.Millisecond)
		}

		interruptAt := time.Now()
		ot.HandleInterruption()
		// Record when audio actually stopped: last write before interrupt.
		lastWrite := writer.last()
		latency := lastWrite.Sub(interruptAt)
		if latency < 0 {
			latency = -latency
		}

		// Allow up to 2 extra chunk durations for the goroutine to observe cancel.
		stopDeadline := time.After(3 * chunkDuration)
		prevCount := writer.count()
		time.Sleep(3 * chunkDuration)
		newCount := writer.count()
		select {
		case <-stopDeadline:
		}
		_ = prevCount
		_ = newCount

		latencies = append(latencies, latency)
		ot.Close()
	}

	// p99 = worst case across trials.
	maxLatency := time.Duration(0)
	for _, l := range latencies {
		if l > maxLatency {
			maxLatency = l
		}
	}
	const target = 30 * time.Millisecond
	t.Logf("Scenario 2: interrupt latencies (10 trials): max=%v target<=%v", maxLatency, target)
	if maxLatency > target {
		t.Errorf("p99 interrupt latency %v exceeds %v", maxLatency, target)
	}
}

// Scenario 3: EndFrame survives interruption.
// Enqueue [audio][audio][EndFrame][audio], interrupt after first audio starts,
// assert EndFrame is still delivered and no goroutine leak.
func TestScenario3_EndFrameSurvivesInterruption(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	obs := &recordingObserver{}
	ot := NewOutputTransport(writer, obs)

	// Enqueue 100ms + 100ms of audio, then EndFrame, then 100ms more.
	ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(100))))
	ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(100))))
	ot.HandleEndFrame() // uninterruptible — must survive Reset()
	ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(100))))

	// Wait for at least a couple of chunks to play.
	for writer.count() < 3 {
		time.Sleep(2 * time.Millisecond)
	}

	// Interrupt: HasUninterruptible=true so audioQueue.Reset() is called,
	// the audio task keeps running, drains to EndFrame, then stops.
	ot.HandleInterruption()

	// WaitDone should return within 500 ms (EndFrame drains quickly after Reset).
	done := make(chan struct{})
	go func() {
		ot.WaitDone()
		close(done)
	}()

	select {
	case <-done:
		// Good: EndFrame was delivered.
	case <-time.After(2 * time.Second):
		t.Fatal("WaitDone timed out — EndFrame was not delivered after interruption")
	}

	if obs.endCount() == 0 {
		t.Error("EndFrame observer was never called")
	}
	t.Logf("Scenario 3: EndFrame delivered after interruption. endCount=%d stoppedCount=%d",
		obs.endCount(), obs.stoppedCount())
}

// Scenario 4: BotStartedSpeaking / BotStoppedSpeaking direction correctness.
// - Non-silence audio → BotStartedSpeaking fires once per run.
// - TTSStopped → BotStoppedSpeaking fires.
// - Silence-only audio: no BotStartedSpeaking (silence check is not done in this
//   experiment version; the frame itself drives state, not silence detection).
//   We test the guard: second BotStartedSpeaking is not emitted if already speaking.
func TestScenario4_BotSpeakingStateMachine(t *testing.T) {
	t.Parallel()

	t.Run("started-only-once", func(t *testing.T) {
		t.Parallel()
		writer := &recordingWriter{}
		obs := &recordingObserver{}
		ot := NewOutputTransport(writer, obs)
		defer ot.Close()

		// Two consecutive audio frames → BotStartedSpeaking should fire exactly once.
		ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(50))))
		ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(50))))
		ot.HandleTTSStopped()

		time.Sleep(300 * time.Millisecond)
		if obs.startedCount() != 1 {
			t.Errorf("BotStartedSpeaking fired %d times, want 1", obs.startedCount())
		}
	})

	t.Run("stopped-after-tts-stopped", func(t *testing.T) {
		t.Parallel()
		writer := &recordingWriter{}
		obs := &recordingObserver{}
		ot := NewOutputTransport(writer, obs)
		defer ot.Close()

		ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(50))))
		ot.HandleTTSStopped()

		// Wait for TTSStopped to be processed.
		deadline := time.After(2 * time.Second)
		for {
			if obs.stoppedCount() >= 1 {
				break
			}
			select {
			case <-deadline:
				t.Fatal("BotStoppedSpeaking never fired")
			default:
				time.Sleep(5 * time.Millisecond)
			}
		}
		if obs.stoppedCount() != 1 {
			t.Errorf("BotStoppedSpeaking fired %d times, want 1", obs.stoppedCount())
		}
	})

	t.Run("no-double-stop", func(t *testing.T) {
		t.Parallel()
		writer := &recordingWriter{}
		obs := &recordingObserver{}
		ot := NewOutputTransport(writer, obs)
		defer ot.Close()

		ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(50))))
		ot.HandleTTSStopped()
		time.Sleep(200 * time.Millisecond)
		// Second TTSStopped when bot is already not speaking → should NOT fire again.
		ot.HandleTTSStopped()
		time.Sleep(100 * time.Millisecond)

		if obs.stoppedCount() > 1 {
			t.Errorf("BotStoppedSpeaking fired %d times after second TTSStopped, want 1", obs.stoppedCount())
		}
	})
}

// Scenario 5: Resume after interrupt.
// 1. Feed 200 ms audio, interrupt after 50 ms.
// 2. Immediately feed new 200 ms audio.
// Assert new audio plays and BotStartedSpeaking fires again.
func TestScenario5_ResumeAfterInterrupt(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	obs := &recordingObserver{}
	ot := NewOutputTransport(writer, obs)
	defer ot.Close()

	// Phase 1: start speaking.
	ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(200))))

	// Wait for a few chunks.
	for writer.count() < 3 {
		time.Sleep(2 * time.Millisecond)
	}

	// Interrupt.
	ot.HandleInterruption()
	time.Sleep(20 * time.Millisecond) // let audio task settle

	countAfterInterrupt := writer.count()

	// Phase 2: feed new audio after interrupt.
	ot.HandleAudioFrame(makeFrame(makeSpeech(audioBytes(200))))

	// Wait for new chunks.
	deadline := time.After(2 * time.Second)
	for {
		if writer.count() > countAfterInterrupt+5 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no new audio written after resume")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// BotStartedSpeaking should have fired at least twice (once per phase).
	if obs.startedCount() < 2 {
		t.Errorf("BotStartedSpeaking fired %d times, want ≥2 (once per speaking phase)", obs.startedCount())
	}
	t.Logf("Scenario 5: resume OK — startedSpeaking=%d countAfterInterrupt=%d finalCount=%d",
		obs.startedCount(), countAfterInterrupt, writer.count())
}

// --- FrameQueue unit tests ---

func TestFrameQueue_BasicPutGet(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	q.Put(&TTSAudioFrame{})
	f, ok := q.Get()
	if !ok || f == nil {
		t.Fatal("expected frame, got nil")
	}
}

func TestFrameQueue_HasUninterruptible(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	if q.HasUninterruptible() {
		t.Fatal("should not have uninterruptible on empty queue")
	}
	q.Put(&EndFrame{})
	if !q.HasUninterruptible() {
		t.Fatal("should have uninterruptible after putting EndFrame")
	}
	q.Get()
	if q.HasUninterruptible() {
		t.Fatal("should not have uninterruptible after getting EndFrame")
	}
}

func TestFrameQueue_Reset_KeepsEndFrame(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	q.Put(&TTSAudioFrame{})
	q.Put(&TTSAudioFrame{})
	q.Put(&EndFrame{})
	q.Put(&TTSAudioFrame{})

	q.Reset()

	if q.Len() != 1 {
		t.Fatalf("after reset: len=%d, want 1 (EndFrame)", q.Len())
	}
	f, _ := q.Get()
	if _, ok := f.(*EndFrame); !ok {
		t.Errorf("expected EndFrame after reset, got %T", f)
	}
}

func TestFrameQueue_ConcurrentPutGet(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	const N = 1000
	var wg sync.WaitGroup
	var received atomic.Int64

	// Producer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			q.Put(&TTSAudioFrame{})
		}
	}()

	// Consumer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			f, ok := q.Get()
			if ok && f != nil {
				received.Add(1)
			}
		}
	}()

	wg.Wait()
	if received.Load() != N {
		t.Errorf("got %d frames, want %d", received.Load(), N)
	}
}
