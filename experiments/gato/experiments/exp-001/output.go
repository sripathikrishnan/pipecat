package main

import (
	"context"
	"sync"
	"time"
)

const (
	sampleRate    = 48000
	channels      = 1
	bytesPerSample = 2 // s16le

	// chunkDuration is the pacing interval — each audio task iteration
	// sends exactly this much audio then sleeps this long.
	// 10 ms matches pipecat's default (audio_out_10ms_chunks=1).
	chunkDuration = 10 * time.Millisecond

	// chunkSize is the byte count per 10 ms chunk at 48 kHz s16le mono.
	chunkSize = sampleRate / 100 * channels * bytesPerSample // 960 bytes
)

// AudioWriter is the interface EXP-001 plugs a Pion WriteSample into.
// In unit tests this is replaced by a recording mock.
type AudioWriter interface {
	WriteAudio(pcm []byte) error
}

// FrameObserver receives upstream/downstream events emitted by the output transport.
// In tests, both directions go into the same recorder; in production these would
// be pushed into the pipeline's upstream and downstream channels.
type FrameObserver interface {
	OnBotStartedSpeaking()
	OnBotStoppedSpeaking()
	OnEndFrame()
}

// OutputTransport implements the pipecat BaseOutputTransport + MediaSender
// for the Gato Go runtime.
//
// External inputs (all goroutine-safe):
//   - HandleAudioFrame(f) — buffer incoming TTS audio, chunk into 10 ms pieces
//   - HandleTTSStopped()  — signal TTS utterance complete
//   - HandleEndFrame()    — enqueue EndFrame (uninterruptible) for clean shutdown
//   - HandleInterruption() — interrupt: drain queue or reset, stop bot-speaking
//
// Internal goroutine:
//   - audioTask — dequeues 10 ms chunks, writes to AudioWriter, sleeps 10 ms
type OutputTransport struct {
	writer   AudioWriter
	observer FrameObserver

	// audioQueue holds chunked AudioFrames, TTSStoppedFrames, and EndFrames.
	audioQueue *FrameQueue

	// audioBuffer accumulates incoming (possibly large) TTS blobs until
	// we have a full chunk.  Protected by mu.
	mu          sync.Mutex
	audioBuffer []byte

	// bot-speaking state machine — guarded by mu.
	botSpeaking bool

	// audioTask lifecycle
	taskMu   sync.Mutex
	taskCtx  context.Context
	taskStop context.CancelFunc
	taskDone chan struct{}
}

func NewOutputTransport(w AudioWriter, obs FrameObserver) *OutputTransport {
	t := &OutputTransport{
		writer:   w,
		observer: obs,
	}
	t.startAudioTask()
	return t
}

// Close shuts the transport down. Safe to call after EndFrame has been enqueued
// but can also be used to force-close.
func (t *OutputTransport) Close() {
	t.stopAudioTask()
}

// --- Public API (goroutine-safe) ---

// HandleAudioFrame accepts a (possibly large) TTS audio blob, buffers it, and
// enqueues 10 ms chunks as they become complete.
func (t *OutputTransport) HandleAudioFrame(f *TTSAudioFrame) {
	t.mu.Lock()
	t.audioBuffer = append(t.audioBuffer, f.Audio...)
	for len(t.audioBuffer) >= chunkSize {
		chunk := make([]byte, chunkSize)
		copy(chunk, t.audioBuffer[:chunkSize])
		t.audioBuffer = t.audioBuffer[chunkSize:]
		t.mu.Unlock()

		t.audioQueue.Put(&TTSAudioFrame{
			AudioFrame: AudioFrame{Audio: chunk, SampleRate: sampleRate},
			Text:       f.Text,
		})

		t.mu.Lock()
	}
	t.mu.Unlock()
}

// HandleTTSStopped enqueues a TTSStoppedFrame so the audio task triggers
// BotStoppedSpeaking after the last audio chunk drains.
func (t *OutputTransport) HandleTTSStopped() {
	t.audioQueue.Put(&TTSStoppedFrame{})
}

// HandleEndFrame enqueues the uninterruptible EndFrame. The audio task will
// break its loop when it dequeues it, and the transport shuts down.
func (t *OutputTransport) HandleEndFrame() {
	t.audioQueue.Put(&EndFrame{})
}

// HandleInterruption handles an incoming InterruptionFrame.
//
// Decision logic (matches pipecat exactly):
//
//	if HasUninterruptible → Reset() the queue (keeps EndFrame, drains audio)
//	                         audio task keeps running to drain the EndFrame
//	else                  → cancel audio task + restart it (clean slate)
//
// Either way, BotStoppedSpeaking is emitted if the bot was speaking.
func (t *OutputTransport) HandleInterruption() {
	if t.audioQueue.HasUninterruptible() {
		t.audioQueue.Reset()
		// Audio task continues — it will drain the remaining EndFrame.
	} else {
		t.stopAudioTask()
		t.startAudioTask()
	}
	t.botStoppedSpeaking()
}

// WaitDone blocks until the audio task exits (EndFrame processed or Close called).
func (t *OutputTransport) WaitDone() {
	t.taskMu.Lock()
	done := t.taskDone
	t.taskMu.Unlock()
	if done != nil {
		<-done
	}
}

// --- Bot-speaking state machine ---

func (t *OutputTransport) botStartedSpeaking() {
	t.mu.Lock()
	already := t.botSpeaking
	t.botSpeaking = true
	t.mu.Unlock()
	if !already {
		t.observer.OnBotStartedSpeaking()
	}
}

func (t *OutputTransport) botStoppedSpeaking() {
	t.mu.Lock()
	already := t.botSpeaking
	t.botSpeaking = false
	t.audioBuffer = t.audioBuffer[:0] // discard sub-chunk leftover
	t.mu.Unlock()
	if already {
		t.observer.OnBotStoppedSpeaking()
	}
}

// --- Audio task goroutine ---

func (t *OutputTransport) startAudioTask() {
	t.taskMu.Lock()
	defer t.taskMu.Unlock()

	t.audioQueue = NewFrameQueue()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.taskCtx = ctx
	t.taskStop = cancel
	t.taskDone = done

	go t.audioTaskRun(ctx, done)
}

func (t *OutputTransport) stopAudioTask() {
	t.taskMu.Lock()
	cancel := t.taskStop
	done := t.taskDone
	q := t.audioQueue
	t.taskMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if q != nil {
		q.Close() // unblock any blocked Get()
	}
	if done != nil {
		<-done // wait for goroutine to exit
	}
}

func (t *OutputTransport) audioTaskRun(ctx context.Context, done chan struct{}) {
	defer close(done)

	for {
		frame, ok := t.audioQueue.Get()
		if !ok {
			// Queue closed (stopAudioTask called).
			return
		}

		// Check if we should stop before processing.
		select {
		case <-ctx.Done():
			return
		default:
		}

		switch f := frame.(type) {
		case *TTSAudioFrame:
			t.botStartedSpeaking()

			if err := t.writer.WriteAudio(f.Audio); err != nil {
				// Non-fatal: log and continue. In production this would push ErrorFrame.
				continue
			}

			// Pace: sleep exactly one chunk duration so we push at real-time rate.
			// This is the critical difference from aiortc (pull) vs Pion (push).
			timer := time.NewTimer(chunkDuration)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

		case *TTSStoppedFrame:
			t.botStoppedSpeaking()

		case *EndFrame:
			t.observer.OnEndFrame()
			return
		}
	}
}
