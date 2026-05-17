// Package exp005 — EXP-005: FrameProcessor Priority Queue
//
// LEARNING: Nested `select` does NOT guarantee strict priority in Go. When both
// systemCh and dataCh have items ready, Go's `select` picks one at RANDOM.
// Testing confirmed: nested select gives SystemFrame priority only ~0.4% of the time
// when dataCh is already full.
//
// SOLUTION: mutex-backed priority queue. The `priorityQueue` struct holds separate
// slices for system and data frames. Pop always returns system frames first.
// This is the Go equivalent of Python's asyncio.PriorityQueue.
package main

import (
	"context"
	"sync"
)

// Frame interface
type Frame interface {
	IsSystem() bool
	FrameID() int
}

// DataFrameImpl is a normal data frame (low priority).
type DataFrameImpl struct{ ID int }

func (f *DataFrameImpl) IsSystem() bool  { return false }
func (f *DataFrameImpl) FrameID() int    { return f.ID }

// SystemFrameImpl is a high-priority system frame (e.g., InterruptionFrame).
type SystemFrameImpl struct{ ID int }

func (f *SystemFrameImpl) IsSystem() bool  { return true }
func (f *SystemFrameImpl) FrameID() int    { return f.ID }

// priorityQueue is a mutex-backed queue with strict system-frame priority.
// Pop always returns a system frame if any are queued, otherwise returns a data frame.
type priorityQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	system []Frame
	data   []Frame
	closed bool
}

func newPriorityQueue() *priorityQueue {
	q := &priorityQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *priorityQueue) push(f Frame) {
	q.mu.Lock()
	if f.IsSystem() {
		q.system = append(q.system, f)
	} else {
		q.data = append(q.data, f)
	}
	q.cond.Signal()
	q.mu.Unlock()
}

// pop returns the next frame: system frames have absolute priority over data frames.
// Blocks until a frame is available, ctx is done, or the queue is closed.
func (q *priorityQueue) pop(ctx context.Context) (Frame, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.system) == 0 && len(q.data) == 0 && !q.closed {
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

	// System frames always first.
	if len(q.system) > 0 {
		f := q.system[0]
		q.system = q.system[1:]
		return f, nil
	}
	if len(q.data) > 0 {
		f := q.data[0]
		q.data = q.data[1:]
		return f, nil
	}
	return nil, nil // closed
}

func (q *priorityQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Expose dataChanCap for the priority test (how many data frames to fill the queue with).
const dataChanCap = 256

// FrameProcessor is a pipeline element with STRICT SystemFrame priority via
// a mutex-backed priority queue.
type FrameProcessor struct {
	name    string
	queue   *priorityQueue
	next    *FrameProcessor
	process func(Frame)

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func NewFrameProcessor(name string, fn func(Frame)) *FrameProcessor {
	return &FrameProcessor{
		name:    name,
		queue:   newPriorityQueue(),
		process: fn,
	}
}

func (fp *FrameProcessor) Start(parentCtx context.Context) {
	ctx, cancel := context.WithCancel(parentCtx)
	fp.cancel = cancel
	fp.wg.Add(1)

	// Wake pop() when context is cancelled.
	go func() {
		<-ctx.Done()
		fp.queue.cond.Broadcast()
	}()

	go fp.run(ctx)
}

func (fp *FrameProcessor) Stop() {
	if fp.cancel != nil {
		fp.cancel()
	}
	fp.queue.close()
	fp.wg.Wait()
}

// PushFrame enqueues a frame. Always succeeds (unbounded).
func (fp *FrameProcessor) PushFrame(f Frame) {
	fp.queue.push(f)
}

func (fp *FrameProcessor) run(ctx context.Context) {
	defer fp.wg.Done()
	for {
		f, err := fp.queue.pop(ctx)
		if err != nil || f == nil {
			return
		}
		fp.deliver(f)
	}
}

func (fp *FrameProcessor) deliver(f Frame) {
	if fp.process != nil {
		fp.process(f)
	}
	if fp.next != nil {
		fp.next.PushFrame(f)
	}
}
