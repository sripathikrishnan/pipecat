package main

import (
	"sync"
)

// FrameQueue is a thread-safe FIFO queue for Frame objects.
//
// Key properties vs plain Go channels:
//   - O(1) HasUninterruptible check — no scan required on interrupt.
//   - Reset() drains non-uninterruptible items under the mutex, keeping
//     EndFrame and other uninterruptible items intact.
//   - Get() blocks until an item is available or the queue is closed.
type FrameQueue struct {
	mu               sync.Mutex
	cond             *sync.Cond
	items            []Frame
	nUninterruptible int // count of UninterruptibleFrame items currently enqueued
	closed           bool
}

func NewFrameQueue() *FrameQueue {
	q := &FrameQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Put enqueues a frame. Safe to call concurrently.
func (q *FrameQueue) Put(f Frame) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if f.IsUninterruptible() {
		q.nUninterruptible++
	}
	q.items = append(q.items, f)
	q.cond.Signal()
}

// Get blocks until a frame is available, then removes and returns it.
// Returns (nil, false) if the queue is closed and empty.
func (q *FrameQueue) Get() (Frame, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return nil, false
	}
	f := q.items[0]
	q.items = q.items[1:]
	if f.IsUninterruptible() {
		q.nUninterruptible--
	}
	return f, true
}

// HasUninterruptible returns true if any EndFrame (or other uninterruptible frame)
// is currently enqueued. O(1).
func (q *FrameQueue) HasUninterruptible() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.nUninterruptible > 0
}

// Reset drains all non-uninterruptible items, keeping uninterruptible ones.
// Used on interruption when an EndFrame is still pending.
func (q *FrameQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.items[:0]
	for _, f := range q.items {
		if f.IsUninterruptible() {
			kept = append(kept, f)
		}
	}
	q.items = kept
	// nUninterruptible is unchanged — all uninterruptible items are still there.
}

// Len returns the current queue depth.
func (q *FrameQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Close unblocks any blocked Get() callers and marks the queue as done.
func (q *FrameQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}
