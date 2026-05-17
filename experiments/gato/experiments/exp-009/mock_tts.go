package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// MockTTS reads from a pre-recorded response file and feeds chunks to an AudioQueue
// at the real-time rate (10ms chunk size, one chunk per 10ms).
// This simulates the full output transport pipeline without Pion.
//
// Audio format: 48 kHz mono int16.
// Chunk size: 960 bytes = 480 samples = 10ms at 48 kHz mono int16.
type MockTTS struct {
	audioData []byte
	chunkSize int     // 960 bytes = 10ms at 48kHz mono int16
	paceMs    float64 // milliseconds between chunks (default 10.0)
}

// newMockTTS loads TTS audio from path. Returns error if the file cannot be read.
func newMockTTS(path string) (*MockTTS, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load TTS audio %q: %w", path, err)
	}
	return &MockTTS{
		audioData: data,
		chunkSize: 960, // 480 samples × 2 bytes = 10ms at 48kHz mono int16
		paceMs:    10.0,
	}, nil
}

// SetPaceMs overrides the inter-chunk pacing. Used by failure injection tests
// (e.g., set to 20.0 for 0.5× rate).
func (m *MockTTS) SetPaceMs(ms float64) {
	m.paceMs = ms
}

// Play enqueues all audio chunks into the AudioQueue at real-time pacing.
// Uses monotonic clock targeting to avoid cumulative drift.
// Calls onFirstByte() (if non-nil) when the first chunk is enqueued.
// Returns when all chunks are sent or ctx is cancelled.
func (m *MockTTS) Play(ctx context.Context, q *AudioQueue, onFirstByte func()) error {
	if len(m.audioData) == 0 {
		return nil
	}

	pace := time.Duration(m.paceMs * float64(time.Millisecond))
	target := time.Now()
	firstByte := true

	for offset := 0; offset < len(m.audioData); offset += m.chunkSize {
		end := offset + m.chunkSize
		if end > len(m.audioData) {
			end = len(m.audioData)
		}
		chunk := m.audioData[offset:end]

		// Copy chunk so the queue owns an independent slice.
		buf := make([]byte, len(chunk))
		copy(buf, chunk)

		if firstByte {
			if onFirstByte != nil {
				onFirstByte()
			}
			firstByte = false
		}

		// Enqueue; if full, wait briefly and retry or bail on ctx.
		for !q.Put(buf) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				// Queue full — wait one chunk duration before retrying.
				time.Sleep(pace)
			}
		}

		// Advance monotonic target.
		target = target.Add(pace)
		delay := time.Until(target)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}
