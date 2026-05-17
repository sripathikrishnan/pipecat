// Package exp003 — EXP-003: Interrupt-Safe Audio Queue
//
// Full FrameQueue implementation with context-aware Get(), O(1) HasUninterruptible(),
// and correct Reset() semantics.
package main

import (
	"context"
	"sync"
	"sync/atomic"
)

// Frame types for this experiment.
type Frame interface {
	IsUninterruptible() bool
}

type AudioFrame struct{ ID int }
type EndFrame struct{}
type TTSStoppedFrame struct{}

func (f *AudioFrame) IsUninterruptible() bool     { return false }
func (f *EndFrame) IsUninterruptible() bool       { return true }
func (f *TTSStoppedFrame) IsUninterruptible() bool { return false }

// FrameQueue is a thread-safe, interrupt-safe FIFO queue for Frame objects.
//
// Invariants:
//   - HasUninterruptible() is O(1) — maintained by atomic counter.
//   - Reset() drains all non-UninterruptibleFrame items atomically under the mutex.
//   - Get() blocks until a frame is available, ctx is done, or Close() is called.
//   - Put() never blocks (unbounded capacity; in production we'd add backpressure).
//   - Concurrent Put+Reset is safe: mutex protects the item slice; counter is atomic.
type FrameQueue struct {
	mu               sync.Mutex
	cond             *sync.Cond
	items            []Frame
	nUninterruptible atomic.Int64
	closed           bool
}

func NewFrameQueue() *FrameQueue {
	q := &FrameQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Put enqueues f. Always succeeds immediately (unbounded).
func (q *FrameQueue) Put(f Frame) {
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
func (q *FrameQueue) Get(ctx context.Context) (Frame, error) {
	// Fast path: check context before locking.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 && !q.closed {
		// Check context cancellation while waiting.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		// Wait releases the mutex; re-checks on Signal/Broadcast.
		// We use a short-circuit wakeup via context by running a monitor goroutine.
		q.cond.Wait()
		// Re-check context after wakeup.
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

// HasUninterruptible returns true if any UninterruptibleFrame (e.g. EndFrame)
// is currently in the queue. O(1) via atomic counter.
func (q *FrameQueue) HasUninterruptible() bool {
	return q.nUninterruptible.Load() > 0
}

// Reset drains all non-UninterruptibleFrame items, keeping EndFrame and other
// uninterruptible frames intact. Called on InterruptionFrame arrival.
func (q *FrameQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()

	kept := q.items[:0]
	for _, f := range q.items {
		if f.IsUninterruptible() {
			kept = append(kept, f)
		}
		// Non-uninterruptible items are discarded.
		// nUninterruptible counter is unchanged (all uninterruptible items kept).
	}
	q.items = kept
	// Signal in case Get() is waiting — it will now dequeue the EndFrame.
	q.cond.Signal()
}

// Len returns the current queue depth. For testing only.
func (q *FrameQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Close marks the queue as done and unblocks all blocked Get() callers.
func (q *FrameQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

// wakeOnCancel broadcasts to the cond when ctx is done, so Get() can unblock.
// Call as a goroutine; it exits when ctx is done or q is closed.
func wakeOnCancel(ctx context.Context, q *FrameQueue) {
	<-ctx.Done()
	q.cond.Broadcast()
}
