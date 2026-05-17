// Package exp007 — EXP-007: [HEARD] End-to-End Interruption
//
// Tests whether the "heard text" mechanism correctly reports which TTS segments
// fully played out before an interruption. Each audio item carries a text label
// on its last chunk; the audio task accumulates heardText when that chunk is
// dequeued and written — not when it is enqueued.
package main

import (
	"strings"
	"sync/atomic"
	"time"
)

const (
	sampleRate     = 48000
	chunkDuration  = 10 * time.Millisecond
	bytesPerSample = 2
	channels       = 1
	// 480 samples × 2 bytes × 1 channel = 960 bytes per 10 ms chunk at 48 kHz mono.
	chunkBytes = sampleRate / 100 * bytesPerSample * channels
)

// audioItem is one 10 ms chunk of audio data.
// segmentText is non-empty only on the last chunk of a TTS segment.
type audioItem struct {
	data        []byte
	segmentText string
	isLastChunk bool
}

// OutputTransport drives audio playback in a goroutine and tracks heard text.
// The zero value is not valid; use newOutputTransport.
type OutputTransport struct {
	queue        chan audioItem
	interrupted  atomic.Int32
	interruptAck chan struct{}
	done         chan struct{}

	// heardText is written exclusively by the audio task goroutine; safe to read
	// after the audio task exits (synchronized via interruptAck or done).
	heardText string

	// playedMs accumulates playback time; use atomic methods for all access.
	playedMs atomic.Int64

	// segmentDone receives the segment text each time a segment fully plays out.
	// Buffered (capacity 8) so the audio task never blocks on an unread signal.
	// Used by tests for precise synchronization without wall-clock timing assumptions.
	segmentDone chan string
}

func newOutputTransport() *OutputTransport {
	t := &OutputTransport{
		// Buffer large enough to hold all pre-generated chunks without blocking.
		queue:        make(chan audioItem, 512),
		interruptAck: make(chan struct{}, 1),
		done:         make(chan struct{}),
		segmentDone:  make(chan string, 8),
	}
	go t.audioTask()
	return t
}

// enqueueSegment splits audio into chunkBytes pieces and enqueues them.
// The last chunk carries segmentText so the audio task knows when a segment completes.
func (t *OutputTransport) enqueueSegment(segmentText string, audio []byte) {
	for i := 0; i < len(audio); i += chunkBytes {
		end := i + chunkBytes
		if end > len(audio) {
			end = len(audio)
		}
		isLast := end >= len(audio)
		seg := ""
		if isLast {
			seg = segmentText
		}
		t.queue <- audioItem{
			data:        audio[i:end],
			segmentText: seg,
			isLastChunk: isLast,
		}
	}
}

// sealQueue closes the audio channel once all segments have been enqueued.
// The audio task exits cleanly when the queue is drained.
func (t *OutputTransport) sealQueue() {
	close(t.queue)
}

// audioTask is the real-time audio playback loop. It runs in its own goroutine.
// The interrupted flag is checked at the top of every iteration so that an
// interruption is noticed within one chunk period (≤ 10 ms) after it is set.
func (t *OutputTransport) audioTask() {
	defer close(t.done)
	for {
		if t.interrupted.Load() == 1 {
			t.interruptAck <- struct{}{}
			return
		}
		item, ok := <-t.queue
		if !ok {
			return // queue sealed and drained
		}
		// Simulate writing to a Pion track (real-time pacing).
		t.playedMs.Add(int64(len(item.data)) * 1000 / int64(bytesPerSample*channels*sampleRate))

		// Mark segment as heard only after its final chunk is "on the wire".
		if item.isLastChunk && item.segmentText != "" {
			t.heardText += item.segmentText + " "
			// Signal test that this segment has fully played out.
			select {
			case t.segmentDone <- item.segmentText:
			default:
				// Buffer full — test is not consuming signals; drop silently.
			}
		}
		time.Sleep(chunkDuration)
	}
}

// handleInterrupt signals the audio task to stop after the current chunk and
// returns the text segments that had fully played out at the moment of interruption.
// Safe to call after the audio task has already exited (queue drained).
func (t *OutputTransport) handleInterrupt() string {
	t.interrupted.Store(1)
	// Either the audio task sends an ack (it was still running) or done is closed
	// (it already exited because the queue was drained). Both cases are safe.
	select {
	case <-t.interruptAck:
	case <-t.done:
	}
	return strings.TrimSpace(t.heardText)
}
