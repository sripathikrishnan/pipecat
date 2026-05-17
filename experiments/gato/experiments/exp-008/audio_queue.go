package main

import (
	"context"
	"sync"
	"sync/atomic"
)

// AudioFrame holds a chunk of raw PCM audio at 48kHz mono s16le.
type AudioFrame struct {
	Data        []byte
	SegmentText string // non-empty on the last chunk of a TTS segment
	IsLastChunk bool
}

// IsUninterruptible returns false — audio frames are dropped on interruption.
func (f *AudioFrame) IsUninterruptible() bool { return false }

// EndAudioFrame signals the audio task to shut down cleanly.
// It is uninterruptible: Reset() keeps it in the queue.
type EndAudioFrame struct{}

// IsUninterruptible returns true — end frames survive interruptions.
func (f *EndAudioFrame) IsUninterruptible() bool { return true }

// StopAudioFrame signals the end of one TTS utterance without shutting down.
// Dropped on interruption.
type StopAudioFrame struct{}

// IsUninterruptible returns false — stop frames are dropped on interruption.
func (f *StopAudioFrame) IsUninterruptible() bool { return false }

// QueueFrame is the union type for the audio queue.
type QueueFrame interface {
	IsUninterruptible() bool
}

// AudioQueue is a thread-safe, interrupt-safe FIFO queue for QueueFrame objects.
//
// Invariants:
//   - HasUninterruptible() is O(1) via atomic counter.
//   - Reset() drains all non-uninterruptible items atomically under the mutex.
//   - Get() blocks until a frame is available, ctx is done, or Close() is called.
//   - Put() never blocks (unbounded capacity).
type AudioQueue struct {
	mu               sync.Mutex
	cond             *sync.Cond
	items            []QueueFrame
	nUninterruptible atomic.Int64
	closed           bool
}

// NewAudioQueue allocates and initialises an AudioQueue.
func NewAudioQueue() *AudioQueue {
	q := &AudioQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Put enqueues f. Always succeeds immediately (unbounded).
func (q *AudioQueue) Put(f QueueFrame) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if f.IsUninterruptible() {
		q.nUninterruptible.Add(1)
	}
	q.items = append(q.items, f)
	q.cond.Signal()
}

// Get blocks until a frame is available, ctx is done, or Close() is called.
// Returns (nil, ctx.Err()) on context cancellation.
// Returns (nil, nil) if the queue is closed and empty.
func (q *AudioQueue) Get(ctx context.Context) (QueueFrame, error) {
	// Fast path: check context before locking.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 && !q.closed {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		q.cond.Wait()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}

	if len(q.items) == 0 {
		return nil, nil // closed and empty
	}

	f := q.items[0]
	q.items = q.items[1:]
	if f.IsUninterruptible() {
		q.nUninterruptible.Add(-1)
	}
	return f, nil
}

// HasUninterruptible returns true if any uninterruptible frame is in the queue. O(1).
func (q *AudioQueue) HasUninterruptible() bool {
	return q.nUninterruptible.Load() > 0
}

// Reset drains all non-uninterruptible items, keeping EndAudioFrame intact.
func (q *AudioQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()

	kept := q.items[:0]
	for _, f := range q.items {
		if f.IsUninterruptible() {
			kept = append(kept, f)
		}
	}
	q.items = kept
	q.cond.Signal()
}

// Len returns the current queue depth (for testing).
func (q *AudioQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Close marks the queue as done and unblocks all blocked Get() callers.
func (q *AudioQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

// wakeOnCancel broadcasts to the cond when ctx is done, so Get() can unblock.
// Run as a goroutine; exits when ctx is done.
func wakeOnCancel(ctx context.Context, q *AudioQueue) {
	<-ctx.Done()
	q.cond.Broadcast()
}
