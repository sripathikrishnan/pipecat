// Package exp001 — EXP-001: Output Transport End-to-End
// Minimal frame types mirroring pipecat's frame model.
package main

// FrameKind distinguishes transport priority tiers.
type FrameKind int

const (
	KindData   FrameKind = iota // normal data frames — go through audio queue
	KindSystem                  // system frames — bypass data queue (high priority)
)

// Frame is the base interface for all pipeline data units.
type Frame interface {
	Kind() FrameKind
	IsUninterruptible() bool // if true, survives FrameQueue.Reset()
}

// --- Audio frames ---

// AudioFrame carries raw PCM audio: s16le, mono, 48 kHz.
// A single AudioFrame may be anywhere from 10 ms to several seconds.
type AudioFrame struct {
	Audio      []byte
	SampleRate int
}

func (f *AudioFrame) Kind() FrameKind       { return KindData }
func (f *AudioFrame) IsUninterruptible() bool { return false }

// TTSAudioFrame is AudioFrame produced by TTS — drives bot_speaking state machine.
type TTSAudioFrame struct {
	AudioFrame
	// Text is the TTS segment this audio derives from (for [HEARD] tracking).
	Text string
}

// TTSStoppedFrame signals that the TTS provider finished its current utterance.
// The output transport uses this to trigger BotStoppedSpeaking.
type TTSStoppedFrame struct{}

func (f *TTSStoppedFrame) Kind() FrameKind       { return KindData }
func (f *TTSStoppedFrame) IsUninterruptible() bool { return false }

// --- Control frames ---

// EndFrame shuts down the pipeline cleanly. Uninterruptible: it survives Reset().
type EndFrame struct{}

func (f *EndFrame) Kind() FrameKind       { return KindData }
func (f *EndFrame) IsUninterruptible() bool { return true }

// InterruptionFrame is a system frame — arrives out-of-band via the system channel.
type InterruptionFrame struct{}

func (f *InterruptionFrame) Kind() FrameKind       { return KindSystem }
func (f *InterruptionFrame) IsUninterruptible() bool { return false }

// --- Observation frames (pushed upstream/downstream by the output transport) ---

// BotStartedSpeakingFrame is emitted when non-silence audio begins playing.
type BotStartedSpeakingFrame struct{}

func (f *BotStartedSpeakingFrame) Kind() FrameKind       { return KindSystem }
func (f *BotStartedSpeakingFrame) IsUninterruptible() bool { return false }

// BotStoppedSpeakingFrame is emitted when audio finishes or is interrupted.
type BotStoppedSpeakingFrame struct{}

func (f *BotStoppedSpeakingFrame) Kind() FrameKind       { return KindSystem }
func (f *BotStoppedSpeakingFrame) IsUninterruptible() bool { return false }
