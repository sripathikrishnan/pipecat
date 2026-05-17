package main

import (
	"context"
	"time"
)

// MockSTT simulates an STT service. After a fixed delay from Finalize(),
// it returns a canned transcript. This isolates the CPU pipeline from real
// network latency, allowing measurement of the pure pipeline overhead.
type MockSTT struct {
	delay      time.Duration
	transcript string
	slowMode   bool // if true, 0.5× rate (used by failure injection)
}

// newMockSTT creates a MockSTT with a 200ms simulated transcription delay.
func newMockSTT() *MockSTT {
	return &MockSTT{
		delay:      200 * time.Millisecond,
		transcript: "hello world",
	}
}

// RecordAudio is called with each 20ms audio chunk during a user turn.
// In the mock, audio is discarded — we only simulate the delay.
func (m *MockSTT) RecordAudio(pcm []byte) {}

// SetDelay overrides the simulated STT finalization delay.
// Used by failure injection tests (e.g., set to 5s to simulate timeout).
func (m *MockSTT) SetDelay(d time.Duration) {
	m.delay = d
}

// Finalize simulates STT finalization delay and returns the transcript.
// Respects ctx cancellation — returns ctx.Err() if cancelled before the delay elapses.
func (m *MockSTT) Finalize(ctx context.Context) (string, error) {
	select {
	case <-time.After(m.delay):
		return m.transcript, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
